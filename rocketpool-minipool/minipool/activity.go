package minipool

import (
    "encoding/hex"
    "encoding/json"
    "errors"
    "fmt"
    "log"

    "github.com/fatih/color"

    "github.com/rocket-pool/smartnode/shared/services"
    beaconchain "github.com/rocket-pool/smartnode/shared/services/beacon-chain"
)


// Config
const ACTIVITY_LOG_COLOR = color.FgBlue


// Activity process
type ActivityProcess struct {
    c func(a ...interface{}) string
    p *services.Provider
    minipool *Minipool
    validatorActive bool
    stop chan struct{}
    done chan struct{}
}


/**
 * Start beacon activity process
 */
func StartActivityProcess(p *services.Provider, minipool *Minipool, done chan struct{}) {

    // Initialise process
    process := &ActivityProcess{
        c: color.New(ACTIVITY_LOG_COLOR).SprintFunc(),
        p: p,
        minipool: minipool,
        validatorActive: false,
        stop: make(chan struct{}),
        done: done,
    }

    // Start
    process.start()

}


/**
 * Start process
 */
func (p *ActivityProcess) start() {

    // Subscribe to beacon chain events
    connectedChannel := make(chan interface{})
    messageChannel := make(chan interface{})
    p.p.Publisher.AddSubscriber("beacon.client.connected", connectedChannel)
    p.p.Publisher.AddSubscriber("beacon.client.message", messageChannel)

    // Handle beacon chain events while subscribed
    go (func() {
        subscribed := true
        for subscribed {
            select {
                case <-connectedChannel:
                    p.onBeaconClientConnected()
                case eventData := <-messageChannel:
                    event := (eventData).(struct{Client *beaconchain.Client; Message []byte})
                    p.onBeaconClientMessage(event.Message)
                case <-p.stop:
                    p.p.Publisher.RemoveSubscriber("beacon.client.connected", connectedChannel)
                    p.p.Publisher.RemoveSubscriber("beacon.client.message", messageChannel)
                    subscribed = false
            }
        }
    })()

    // Block thread until done
    select {
        case <-p.stop:
            log.Println(p.c(fmt.Sprintf("Ending validator %s activity process...", hex.EncodeToString(p.minipool.Key.PublicKey.Marshal()))))
            p.done <- struct{}{}
    }

}


/**
 * Handle beacon chain client connections
 */
func (p *ActivityProcess) onBeaconClientConnected() {

    // Request validator status
    if payload, err := json.Marshal(beaconchain.ClientMessage{
        Message: "get_validator_status",
        Pubkey: hex.EncodeToString(p.minipool.Key.PublicKey.Marshal()),
    }); err != nil {
        log.Println(p.c(errors.New("Error encoding get validator status payload: " + err.Error())))
    } else if err := p.p.Beacon.Send(payload); err != nil {
        log.Println(p.c(errors.New("Error sending get validator status message: " + err.Error())))
    }

}


/**
 * Handle beacon chain client messages
 */
func (p *ActivityProcess) onBeaconClientMessage(messageData []byte) {

    // Parse message
    message := new(beaconchain.ServerMessage)
    if err := json.Unmarshal(messageData, message); err != nil {
        log.Println(p.c(errors.New("Error decoding beacon message: " + err.Error())))
        return
    }

    // Handle message by type
    switch message.Message {

        // Validator status
        case "validator_status":

            // Check validator pubkey
            if hex.EncodeToString(p.minipool.Key.PublicKey.Marshal()) != message.Pubkey { break }

            // Handle statuses
            switch message.Status.Code {

                // Inactive
                case "inactive":
                    log.Println(p.c(fmt.Sprintf("Validator %s is inactive, waiting until active to send activity...", message.Pubkey)))
                    p.validatorActive = false

                // Active
                case "active":
                    log.Println(p.c(fmt.Sprintf("Validator %s is active, sending activity...", message.Pubkey)))
                    p.validatorActive = true

                // Exited
                case "exited": fallthrough
                case "withdrawable": fallthrough
                case "withdrawn":
                    log.Println(p.c(fmt.Sprintf("Validator %s has exited, not sending activity...", message.Pubkey)))
                    p.validatorActive = false
                    close(p.stop)

            }

        // Epoch
        case "epoch":

            // Check validator active status, get pubkey string
            if !p.validatorActive { break }
            pubkeyHex := hex.EncodeToString(p.minipool.Key.PublicKey.Marshal())

            // Log activity
            log.Println(p.c(fmt.Sprintf("New epoch, sending activity for validator %s...", pubkeyHex)))

            // Send activity
            if payload, err := json.Marshal(beaconchain.ClientMessage{
                Message: "activity",
                Pubkey: pubkeyHex,
            }); err != nil {
                log.Println(p.c(errors.New("Error encoding activity payload: " + err.Error())))
            } else if err := p.p.Beacon.Send(payload); err != nil {
                log.Println(p.c(errors.New("Error sending activity message: " + err.Error())))
            }

        // Success response
        case "success":
            if message.Action == "process_activity" {
                log.Println(p.c("Processed validator activity successfully..."))
            }

        // Error
        case "error":
            log.Println(p.c("A beacon server error occurred: ", message.Error))

    }

}
