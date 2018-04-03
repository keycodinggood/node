package connection

import (
	"errors"
	log "github.com/cihub/seelog"
	"github.com/mysterium/node/communication"
	"github.com/mysterium/node/identity"
	"github.com/mysterium/node/openvpn"
	"github.com/mysterium/node/openvpn/middlewares/client/bytescount"
	"github.com/mysterium/node/server"
	"github.com/mysterium/node/service_discovery/dto"
	"github.com/mysterium/node/session"
	"github.com/mysterium/node/utils"
)

const managerLogPrefix = "[connection-manager] "

const channelClosed = openvpn.State("")

var (
	// ErrNoConnection error indicates that action applied to manager expects active connection (i.e. disconnect)
	ErrNoConnection = errors.New("no connection exists")
	// ErrAlreadyExists error indicates that aciton applieto to manager expects no active connection (i.e. connect)
	ErrAlreadyExists = errors.New("connection already exists")
	// ErrConnectionCancelled indicates that connection in progress was cancelled by request of api user
	ErrConnectionCancelled = errors.New("connection was cancelled")
	// ErrOpenvpnProcessDied indicates that Connect method didn't reach "Connected" phase due to openvpn error
	ErrOpenvpnProcessDied = errors.New("openvpn process died")
)

type connectionManager struct {
	//these are passed on creation
	mysteriumClient server.Client
	newDialog       DialogCreator
	newVpnClient    VpnClientCreator
	statsKeeper     bytescount.SessionStatsKeeper
	//these are populated by Connect at runtime
	status      ConnectionStatus
	closeAction func()
}

func warnOnClose() {
	log.Warn(managerLogPrefix, "WARNING! Trying to close when there is nothing to close. Possible bug or race condition")
}

// NewManager creates connection manager with given dependencies
func NewManager(mysteriumClient server.Client, dialogCreator DialogCreator,
	vpnClientCreator VpnClientCreator, statsKeeper bytescount.SessionStatsKeeper) *connectionManager {
	return &connectionManager{
		mysteriumClient: mysteriumClient,
		newDialog:       dialogCreator,
		newVpnClient:    vpnClientCreator,
		statsKeeper:     statsKeeper,
		status:          statusNotConnected(),
		closeAction:     warnOnClose,
	}
}

func (manager *connectionManager) Connect(consumerID, providerID identity.Identity) (err error) {
	if manager.status.State != NotConnected {
		return ErrAlreadyExists
	}

	manager.status = statusConnecting()
	defer func() {
		if err != nil {
			manager.status = statusNotConnected()
		}
	}()

	cancelable := utils.NewCancelable()
	manager.closeAction = utils.CallOnce(func() {
		log.Info(managerLogPrefix, "Closing active connection")
		manager.status = statusDisconnecting()
		cancelable.Cancel()
	})

	val, err := cancelable.
		Request(func() (interface{}, error) {
			return manager.findProposalByProviderID(providerID)
		}).
		Call()
	if err != nil {
		return err
	}
	proposal := val.(*dto.ServiceProposal)

	val, err = cancelable.
		Request(func() (interface{}, error) {
			return manager.newDialog(consumerID, providerID, proposal.ProviderContacts[0])
		}).
		Cleanup(utils.SkipOnError(func(val interface{}) {
			val.(communication.Dialog).Close()
		})).
		Call()
	if err != nil {
		return err
	}
	dialog := val.(communication.Dialog)

	val, err = cancelable.
		Request(func() (interface{}, error) {
			return session.RequestSessionCreate(dialog, proposal.ID)
		}).
		Call()
	if err != nil {
		dialog.Close()
		return err
	}
	vpnSession := val.(*session.SessionDto)

	stateChannel := make(chan openvpn.State, 10)
	val, err = cancelable.
		Request(func() (interface{}, error) {
			return manager.startOpenvpnClient(*vpnSession, consumerID, providerID, stateChannel)
		}).
		Cleanup(utils.SkipOnError(func(val interface{}) {
			val.(openvpn.Client).Stop()
		})).
		Call()
	if err != nil {
		dialog.Close()
		return err
	}
	openvpnClient := val.(openvpn.Client)

	err = manager.waitForConnectedState(stateChannel, vpnSession.ID, cancelable.Cancelled)
	if err != nil {
		dialog.Close()
		openvpnClient.Stop()
		return err
	}

	go openvpnClientStopper(openvpnClient, cancelable.Cancelled)
	go openvpnClientWaiter(openvpnClient, dialog)
	go manager.consumeOpenvpnStates(stateChannel, vpnSession.ID)
	return nil
}

func (manager *connectionManager) Status() ConnectionStatus {
	return manager.status
}

func (manager *connectionManager) Disconnect() error {
	if manager.status.State == NotConnected {
		return ErrNoConnection
	}
	manager.closeAction()
	return nil
}

// TODO this can be extraced as depencency later when node selection criteria will be clear
func (manager *connectionManager) findProposalByProviderID(providerID identity.Identity) (*dto.ServiceProposal, error) {
	proposals, err := manager.mysteriumClient.FindProposals(providerID.Address)
	if err != nil {
		return nil, err
	}
	if len(proposals) == 0 {
		err = errors.New("provider has no service proposals")
		return nil, err
	}
	return &proposals[0], nil
}

func openvpnClientStopper(openvpnClient openvpn.Client, cancelRequest utils.CancelChannel) {
	<-cancelRequest
	log.Debug(managerLogPrefix, "Stopping openvpn client")
	err := openvpnClient.Stop()
	if err != nil {
		log.Error(managerLogPrefix, "Failed to stop openvpn client: ", err)
	}
}

func openvpnClientWaiter(openvpnClient openvpn.Client, dialog communication.Dialog) {
	defer dialog.Close()
	err := openvpnClient.Wait()
	if err != nil {
		log.Warn(managerLogPrefix, "Openvpn client exited with error: ", err)
	} else {
		log.Info(managerLogPrefix, "Openvpn client exited")
	}
}

func (manager *connectionManager) startOpenvpnClient(vpnSession session.SessionDto, consumerID, providerID identity.Identity, stateChannel chan openvpn.State) (openvpn.Client, error) {
	openvpnClient, err := manager.newVpnClient(
		vpnSession,
		consumerID,
		providerID,
		channelToStateCallbackAdapter(stateChannel),
	)
	if err != nil {
		return nil, err
	}

	if err = openvpnClient.Start(); err != nil {
		return nil, err
	}

	return openvpnClient, nil
}

func (manager *connectionManager) waitForConnectedState(stateChannel <-chan openvpn.State, sessionID session.SessionID, cancelRequest utils.CancelChannel) error {

	for {
		select {
		case state := <-stateChannel:
			switch state {
			case openvpn.ConnectedState:
				manager.onStateChanged(state, sessionID)
				return nil
			case channelClosed:
				return ErrOpenvpnProcessDied
			default:
				manager.onStateChanged(state, sessionID)
			}
		case <-cancelRequest:
			return ErrConnectionCancelled
		}
	}
}

func (manager *connectionManager) consumeOpenvpnStates(stateChannel <-chan openvpn.State, sessionID session.SessionID) {
	func() {
		for state := range stateChannel {
			manager.onStateChanged(state, sessionID)
		}
		manager.status = statusNotConnected()
		log.Debug(managerLogPrefix, "State updater stopped")
	}()

}

func (manager *connectionManager) onStateChanged(state openvpn.State, sessionID session.SessionID) {
	switch state {
	case openvpn.ConnectedState:
		manager.statsKeeper.MarkSessionStart()
		manager.status = statusConnected(sessionID)
	case openvpn.ExitingState:
		manager.statsKeeper.MarkSessionEnd()
	case openvpn.ReconnectingState:
		manager.status = statusReconnecting()
	}
}
