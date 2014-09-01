package reign

import (
	"errors"
	"fmt"
	"reign/internal"
	"sync"
)

type messageSender interface {
	send(*internal.ClusterMessage) error
	terminate()
}

// remoteMailboxes, which may need a better name, connects the local
// mailboxes with the remote connection. Once a node has connected to a
// remote node, either as the server or a client, the connection becomes
// symmetric and both sides run this.
//
// A given instance is responsible only for maintaining communication with
// a particular node.
type remoteMailboxes struct {
	connection messageSender
	NodeID
	Address
	parent          *mailboxes
	outgoingMailbox *Mailbox
	ClusterLogger
	connectionServer *connectionServer

	// A set-like map that records the remote MailboxID that a local
	// Address is linked to, mapped to the set of local mailboxes that
	// are so linked.
	// The map here starts with Node, so that when a Node goes down,
	// all the mailboxes are nicely sorted out for just that node,
	// then it's the remote mailbox in question, then it's the set of
	// local mailboxes that are subscribed to that remote mailbox.
	linksToRemote map[mailboxID]map[mailboxID]voidtype

	// a debugging function that allows us to examine the messages flowing
	// through
	examineMessages func(interface{}) bool
	// a debugging function that allows us to examine the messages as they
	// are done processing
	doneProcessing func(interface{}) bool

	// a debugging function that allows us to see that a connection has
	// been re-established.
	connectionEstablished func()

	sync.Mutex
	condition *sync.Cond
}

type newExamineMessages struct {
	f func(interface{}) bool
}
type newDoneProcessing struct {
	f func(interface{}) bool
}

func newRemoteMailboxes(connectionServer *connectionServer, mailboxes *mailboxes, logger ClusterLogger, source NodeID) *remoteMailboxes {
	addr, mailbox := mailboxes.newLocalMailbox()
	rm := &remoteMailboxes{
		Address:          addr,
		outgoingMailbox:  mailbox,
		ClusterLogger:    logger,
		parent:           mailboxes,
		NodeID:           source,
		connectionServer: connectionServer,
		linksToRemote:    make(map[mailboxID]map[mailboxID]voidtype)}
	rm.condition = sync.NewCond(&rm.Mutex)
	return rm
}

// awaitConnection waits until the remoteMailboxs have a non-nil connection
func (rm *remoteMailboxes) waitForConnection() {
	rm.Lock()
	for rm.connection == nil {
		rm.condition.Wait()
	}
	rm.Unlock()
}

func (rm *remoteMailboxes) setConnection(ms messageSender) {
	rm.Lock()
	rm.connection = ms
	if rm.connectionEstablished != nil {
		rm.connectionEstablished()
	}
	rm.condition.Broadcast()
	rm.Unlock()
}

func (rm *remoteMailboxes) unsetConnection(ms messageSender) {
	rm.Lock()
	if rm.connection == ms {
		rm.connection = nil
	}
	rm.Unlock()
}

type terminateRemoteMailbox struct{}

func (rm *remoteMailboxes) Stop() {
	rm.Send(terminateRemoteMailbox{})
}

var errNoConnection = errors.New("no connection")

func (rm *remoteMailboxes) send(cm internal.ClusterMessage, desc string) error {
	if rm.connection == nil {
		if rm.ClusterLogger != nil {
			rm.Error("Could send message \"%s\" because there's no connection", desc)
		}
		return errNoConnection
	}
	err := rm.connection.send(&cm)
	if err != nil {
		rm.Error("Error sending msg \"%s\": %s", desc, myString(err))
	}
	return err
}

func (rm *remoteMailboxes) Serve() {
	defer func() {
		for remoteID, localIDs := range rm.linksToRemote {
			for localID := range localIDs {
				// FIXME: sendByID?
				var addr Address
				addr.id = localID
				addr.connectionServer = rm.connectionServer
				addr.Send(MailboxTerminated(remoteID))
			}
		}
		rm.linksToRemote = make(map[mailboxID]map[mailboxID]voidtype)

		if r := recover(); r != nil {
			rm.Error("While handling mailbox, got fatal error (this is a serious bug): %s", myString(r))
			if rm.connection != nil {
				rm.connection.terminate()
			}
			panic(r)
		}
	}()

	var m interface{}
	for {
		if rm.doneProcessing != nil {
			if !rm.doneProcessing(m) {
				rm.doneProcessing = nil
			}
		}

		m = rm.outgoingMailbox.ReceiveNext()

		if rm.examineMessages != nil {
			if !rm.examineMessages(m) {
				rm.examineMessages = nil
			}
		}

		switch msg := m.(type) {
		case internal.OutgoingMailboxMessage:
			rm.send(internal.IncomingMailboxMessage{msg.Target, msg.Message}, "normal message")

		// all of the gob encoding stuff seems to end up with this getting
		// an extra layer of pointer indirection added to it.
		case *internal.IncomingMailboxMessage:
			var addr Address
			addr.id = mailboxID(msg.Target)
			addr.connectionServer = rm.connectionServer
			addr.Send(msg.Message)

		case internal.NotifyRemote:
			// FIXME: if the local addr dies, this never cleans out
			// link. This will eventually be a memory leak.
			// Unfortunately it implies we need another map of local
			// address to their relevant entries and to subscribe to them too.
			remoteID := mailboxID(msg.Remote)
			localID := mailboxID(msg.Local)

			linksToRemote, remoteLinksExist := rm.linksToRemote[remoteID]
			if remoteLinksExist {
				_, thisAddressAlreadyLinked := linksToRemote[localID]
				if thisAddressAlreadyLinked {
					// a no-op; msg.local has already set notify for msg.remote
					continue
				}
			} else {
				linksToRemote = make(map[mailboxID]voidtype)
				rm.linksToRemote[remoteID] = linksToRemote
			}

			if len(linksToRemote) == 0 {
				// Since this is the first link to this particular
				// remote mailbox we are recording, we need to send along
				// the registration message
				err := rm.send(&internal.NotifyNodeOnTerminate{internal.IntMailboxID(remoteID)}, "termination notification")
				if err != nil {
					var addr Address
					addr.id = localID
					addr.connectionServer = rm.connectionServer
					addr.Send(MailboxTerminated(remoteID))
					// FIXME: Really? Panic?
					panic(err)
				}
			}

			linksToRemote[localID] = void

		case internal.UnnotifyRemote:
			remoteID := mailboxID(msg.Remote)
			localID := mailboxID(msg.Local)

			linksToRemote, remoteLinksExist := rm.linksToRemote[remoteID]
			if !remoteLinksExist || len(linksToRemote) == 0 {
				continue
			}

			delete(linksToRemote, localID)

			if len(linksToRemote) == 0 {
				// if that was the last link, we need to unregister from
				// the remote node
				// send does all the error handling I need here
				_ = rm.send(&internal.RemoveNotifyNodeOnTerminate{internal.IntMailboxID(remoteID)}, "remove notify node")
			}

		case *internal.RemoteMailboxTerminated:
			// A remote mailbox has been terminated that we indicated
			// interest in.
			remoteID := mailboxID(msg.IntMailboxID)
			links, linksExist := rm.linksToRemote[remoteID]
			if !linksExist || len(links) == 0 {
				continue
			}

			for subscribed := range links {
				var addr Address
				addr.id = subscribed
				addr.connectionServer = rm.connectionServer
				addr.Send(MailboxTerminated(remoteID))
			}

			delete(rm.linksToRemote, remoteID)

		case *internal.NotifyNodeOnTerminate:
			// this has to be a localID, or we wouldn't be receiving this
			// message
			localID := mailboxID(msg.IntMailboxID)
			var addr Address
			addr.id = localID
			addr.connectionServer = rm.connectionServer
			addr.NotifyAddressOnTerminate(rm.Address)

		case *internal.RemoveNotifyNodeOnTerminate:
			localID := mailboxID(msg.IntMailboxID)
			var addr Address
			addr.id = localID
			addr.connectionServer = rm.connectionServer
			addr.RemoveNotifyAddress(rm.Address)

		// Note this is a local mailbox.
		case MailboxTerminated:
			id, isRealMailbox := AddressID(msg).(mailboxID)
			if isRealMailbox {
				// if we are receiving this, apparently the other side wants to
				// hear about it
				_ = rm.send(&internal.RemoteMailboxTerminated{internal.IntMailboxID(id)}, "mailbox terminated normally")
			} else {
				rm.Trace("Somehow got a mailbox termination for a non-mailboxID: %#v", msg)
			}

		// This allows us to test proper error handling, despite
		// the fact I don't know how to panic any of the above code
		case internal.PanicHandler:
			panic("Panicking as requested due to panic handler")
		case internal.DestroyConnection:
			rm.connection.terminate()

		case newExamineMessages:
			rm.examineMessages = msg.f
		case newDoneProcessing:
			rm.doneProcessing = msg.f

		case terminateRemoteMailbox:
			return

		default:
			fmt.Printf("Unexpected message received: %#v", msg)
			rm.Error("Unexpected message arrived in our node mailbox: %#v", msg)
		}
	}
}
