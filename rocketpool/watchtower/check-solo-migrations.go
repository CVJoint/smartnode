package watchtower

import (
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/rocket-pool/rocketpool-go/minipool"
	"github.com/rocket-pool/rocketpool-go/rocketpool"
	"github.com/rocket-pool/rocketpool-go/types"
	"github.com/rocket-pool/rocketpool-go/utils/eth"
	"github.com/rocket-pool/smartnode/shared/services"
	"github.com/rocket-pool/smartnode/shared/services/beacon"
	"github.com/rocket-pool/smartnode/shared/services/config"
	"github.com/rocket-pool/smartnode/shared/services/state"
	"github.com/rocket-pool/smartnode/shared/services/wallet"
	"github.com/rocket-pool/smartnode/shared/utils/api"
	"github.com/rocket-pool/smartnode/shared/utils/log"
	"github.com/urfave/cli"
)

const (
	soloMigrationCheckThreshold float64 = 0.85 // Fraction of PromotionStakePeriod that can go before a minipool gets scrubbed for not having changed to 0x01
	blsPrefix                   byte    = 0x00
	elPrefix                    byte    = 0x01
	migrationBalanceBuffer      float64 = 0.001
)

type checkSoloMigrations struct {
	c                *cli.Context
	log              log.ColorLogger
	errLog           log.ColorLogger
	cfg              *config.RocketPoolConfig
	w                *wallet.Wallet
	rp               *rocketpool.RocketPool
	ec               rocketpool.ExecutionClient
	bc               beacon.Client
	lock             *sync.Mutex
	isRunning        bool
	generationPrefix string
}

// Create check solo migrations task
func newCheckSoloMigrations(c *cli.Context, logger log.ColorLogger, errorLogger log.ColorLogger) (*checkSoloMigrations, error) {

	// Get services
	cfg, err := services.GetConfig(c)
	if err != nil {
		return nil, err
	}
	w, err := services.GetWallet(c)
	if err != nil {
		return nil, err
	}
	ec, err := services.GetEthClient(c)
	if err != nil {
		return nil, err
	}
	rp, err := services.GetRocketPool(c)
	if err != nil {
		return nil, err
	}
	bc, err := services.GetBeaconClient(c)
	if err != nil {
		return nil, err
	}

	// Return task
	lock := &sync.Mutex{}
	return &checkSoloMigrations{
		c:                c,
		log:              logger,
		errLog:           errorLogger,
		cfg:              cfg,
		w:                w,
		rp:               rp,
		ec:               ec,
		bc:               bc,
		lock:             lock,
		isRunning:        false,
		generationPrefix: "[Solo Migration]",
	}, nil

}

// Start the solo migration checking thread
func (t *checkSoloMigrations) run(state *state.NetworkState, isAtlasDeployed bool) error {

	// Wait for eth clients to sync
	if err := services.WaitEthClientSynced(t.c, true); err != nil {
		return err
	}
	if err := services.WaitBeaconClientSynced(t.c, true); err != nil {
		return err
	}

	// Check if Atlas has been deployed yet
	if !isAtlasDeployed {
		return nil
	}

	// Log
	t.log.Println("Checking for solo migrations...")

	// Check if the check is already running
	t.lock.Lock()
	if t.isRunning {
		t.log.Println("Solo migration check is already running in the background.")
		t.lock.Unlock()
		return nil
	}
	t.lock.Unlock()

	// Run the check
	go func() {
		t.lock.Lock()
		t.isRunning = true
		t.lock.Unlock()
		t.printMessage("Starting solo migration check in a separate thread.")

		err := t.checkSoloMigrations(state)
		if err != nil {
			t.handleError(fmt.Errorf("%s %w", t.generationPrefix, err))
			return
		}

		t.lock.Lock()
		t.isRunning = false
		t.lock.Unlock()
	}()

	// Return
	return nil

}

// Check for solo staker migration validity
func (t *checkSoloMigrations) checkSoloMigrations(state *state.NetworkState) error {

	t.printMessage(fmt.Sprintf("Checking for Beacon slot %d (EL block %d)", state.BeaconSlotNumber, state.ElBlockNumber))
	oneGwei := eth.GweiToWei(1)
	scrubThreshold := time.Duration(state.NetworkDetails.PromotionScrubPeriod.Seconds()*soloMigrationCheckThreshold) * time.Second

	genesisTime := time.Unix(int64(state.BeaconConfig.GenesisTime), 0)
	secondsForSlot := time.Duration(state.BeaconSlotNumber*state.BeaconConfig.SecondsPerSlot) * time.Second
	blockTime := genesisTime.Add(secondsForSlot)

	// Go through each minipool
	threshold := uint64(32000000000)
	buffer := uint64(migrationBalanceBuffer * eth.WeiPerGwei)
	for _, mpd := range state.MinipoolDetails {
		if mpd.Status == types.Dissolved {
			// Ignore minipools that are already dissolved
			continue
		}

		if !mpd.IsVacant {
			// Ignore minipools that aren't vacant
			continue
		}

		// Scrub minipools that aren't seen on Beacon yet
		validator := state.ValidatorDetails[mpd.Pubkey]
		if !validator.Exists {
			t.scrubVacantMinipool(mpd.MinipoolAddress, fmt.Sprintf("minipool %s (pubkey %s) did not exist on Beacon yet, but is required to be active_ongoing for migration", mpd.MinipoolAddress.Hex(), mpd.Pubkey.Hex()))
		}

		// Scrub minipools that are in the wrong state
		if validator.Status != beacon.ValidatorState_ActiveOngoing {
			t.scrubVacantMinipool(mpd.MinipoolAddress, fmt.Sprintf("minipool %s (pubkey %s) was in state %v, but is required to be active_ongoing for migration", mpd.MinipoolAddress.Hex(), mpd.Pubkey.Hex(), validator.Status))
			continue
		}

		// Check the withdrawal credentials
		withdrawalCreds := validator.WithdrawalCredentials
		switch withdrawalCreds[0] {
		case blsPrefix:
			creationTime := time.Unix(mpd.StatusTime.Int64(), 0)
			remainingTime := creationTime.Add(scrubThreshold).Sub(blockTime)
			if remainingTime < 0 {
				t.scrubVacantMinipool(mpd.MinipoolAddress, fmt.Sprintf("minipool timed out (created %s, current time %s, scrubbed after %s)", creationTime, blockTime, scrubThreshold))
				continue
			}
			continue
		case elPrefix:
			if withdrawalCreds != mpd.WithdrawalCredentials {
				t.scrubVacantMinipool(mpd.MinipoolAddress, fmt.Sprintf("withdrawal credentials do not match (expected %s, actual %s)", mpd.WithdrawalCredentials.Hex(), withdrawalCreds.Hex()))
				continue
			}
		default:
			t.scrubVacantMinipool(mpd.MinipoolAddress, fmt.Sprintf("unexpected prefix in withdrawal credentials: %s", withdrawalCreds.Hex()))
			continue
		}

		// Check the balance
		creationBalanceGwei := big.NewInt(0).Div(mpd.PreMigrationBalance, oneGwei).Uint64()
		currentBalance := validator.Balance

		// Add the minipool balance to the Beacon balance in case it already got skimmed
		minipoolBalanceGwei := big.NewInt(0).Div(mpd.Balance, oneGwei).Uint64()
		currentBalance += minipoolBalanceGwei

		if currentBalance < threshold {
			t.scrubVacantMinipool(mpd.MinipoolAddress, fmt.Sprintf("current balance of %d is lower than the threshold of %d", currentBalance, threshold))
			continue
		}
		if currentBalance < (creationBalanceGwei - buffer) {
			t.scrubVacantMinipool(mpd.MinipoolAddress, fmt.Sprintf("current balance of %d is lower than the creation balance of %d, and below the acceptable buffer threshold of %d", currentBalance, creationBalanceGwei, buffer))
			continue
		}

	}

	return nil

}

// Scrub a vacant minipool
func (t *checkSoloMigrations) scrubVacantMinipool(address common.Address, reason string) error {

	// Log
	t.printMessage("=== SCRUBBING SOLO MIGRATION ===")
	t.printMessage(fmt.Sprintf("Minipool: %s", address.Hex()))
	t.printMessage(fmt.Sprintf("Reason:   %s", reason))
	t.printMessage("================================")

	// Make the binding
	mp, err := minipool.NewMinipool(t.rp, address, nil)
	if err != nil {
		return fmt.Errorf("error scrubbing migration of minipool %s: %w", address.Hex(), err)
	}

	// Get transactor
	opts, err := t.w.GetNodeAccountTransactor()
	if err != nil {
		return err
	}

	// Get the gas limit
	gasInfo, err := mp.EstimateVoteScrubGas(opts)
	if err != nil {
		return fmt.Errorf("could not estimate the gas required to scrub the minipool: %w", err)
	}

	// Print the gas info
	maxFee := eth.GweiToWei(getWatchtowerMaxFee(t.cfg))
	if !api.PrintAndCheckGasInfo(gasInfo, false, 0, t.log, maxFee, 0) {
		return nil
	}

	// Set the gas settings
	opts.GasFeeCap = maxFee
	opts.GasTipCap = eth.GweiToWei(getWatchtowerPrioFee(t.cfg))
	opts.GasLimit = gasInfo.SafeGasLimit

	// Cancel the reduction
	hash, err := mp.VoteScrub(opts)
	if err != nil {
		return err
	}

	// Print TX info and wait for it to be included in a block
	err = api.PrintAndWaitForTransaction(t.cfg, hash, t.rp.Client, t.log)
	if err != nil {
		return err
	}

	// Log
	t.log.Printlnf("Successfully voted to scrub minipool %s.", mp.GetAddress().Hex())

	// Return
	return nil

}

func (t *checkSoloMigrations) handleError(err error) {
	t.errLog.Println(err)
	t.errLog.Println("*** Solo migration check failed. ***")
	t.lock.Lock()
	t.isRunning = false
	t.lock.Unlock()
}

// Print a message from the tree generation goroutine
func (t *checkSoloMigrations) printMessage(message string) {
	t.log.Printlnf("%s %s", t.generationPrefix, message)
}
