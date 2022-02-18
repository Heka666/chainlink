package client

import (
	"context"
	"fmt"
)

// NodeState represents the current state of the node
// Node is a FSM
type NodeState int

func (n NodeState) String() string {
	switch n {
	case NodeStateUndialed:
		return "Undialed"
	case NodeStateDialed:
		return "Dialed"
	case NodeStateInvalidChainID:
		return "InvalidChainID"
	case NodeStateAlive:
		return "Alive"
	case NodeStateUnreachable:
		return "Unreachable"
	case NodeStateOutOfSync:
		return "OutOfSync"
	case NodeStateClosed:
		return "Closed"
	default:
		return fmt.Sprintf("NodeState(%d)", n)
	}
}

// GoString prints a prettier state
func (n NodeState) GoString() string {
	return fmt.Sprintf("NodeState%s(%d)", n.String(), n)
}

const (
	// NodeStateUndialed is the first state of a virgin node
	NodeStateUndialed = NodeState(iota)
	// NodeStateDialed is after a node has successfully dialed but before it has verified the correct chain ID
	NodeStateDialed
	// NodeStateInvalidChainID is after chain ID verification failed
	NodeStateInvalidChainID
	// NodeStateAlive is a healthy node after chain ID verification succeeded
	NodeStateAlive
	// NodeStateUnreachable is a node that cannot be dialed or has disconnected
	NodeStateUnreachable
	// NodeStateOutOfSync is a node that is accepting connections but exceeded
	// the failure threshold without sending any new heads. It will be
	// disconnected, then put into a revive loop and re-awakened after redial
	// if a new head arrives
	NodeStateOutOfSync
	// NodeStateClosed is after the connection has been closed and the node is at the end of its lifecycle
	NodeStateClosed
)

// allNodeStates returns all possible states a node can be in
// NOTE: Please update this when adding a new state above
func allNodeStates() []NodeState {
	return []NodeState{NodeStateUndialed, NodeStateDialed, NodeStateInvalidChainID, NodeStateAlive, NodeStateUnreachable, NodeStateOutOfSync, NodeStateClosed}
}

// FSM methods

// State allows reading the current state of the node
func (n *node) State() NodeState {
	n.stateMu.RLock()
	defer n.stateMu.RUnlock()
	return n.state
}

// setState is only used by internal state management methods.
// This is low-level; care should be taken by the caller to ensure the new state is a valid transition.
// State changes should always be synchronous: only one goroutine at a time should change state.
// n.stateMu should not be locked for long periods of time because external clients expect a timely response from n.State()
func (n *node) setState(s NodeState) {
	n.stateMu.Lock()
	defer n.stateMu.Unlock()
	n.state = s
}

// declareXXX methods change the state and pass conrol off the new state
// management goroutine
// TODO: unit test the state transitions

func (n *node) declareAlive() {
	if closed := n.transitionToAlive(); closed {
		return
	}
	n.log.Infow("EVM Node is online", "nodeState", n.State())
	n.wg.Add(1)
	go n.aliveLoop()
}

func (n *node) transitionToAlive() (closed bool) {
	n.stateMu.Lock()
	defer n.stateMu.Unlock()
	switch n.state {
	case NodeStateClosed:
		return true
	case NodeStateDialed:
		n.state = NodeStateAlive
	default:
		panic(fmt.Sprintf("cannot transition from %#v to NodeStateAlive", n.state))
	}
	return false
}

// declareInSync puts a node back into Alive state, allowing it to be used by
// pool consumers again
func (n *node) declareInSync() {
	if closed := n.transitionToInSync(); closed {
		return
	}
	n.log.Infow("EVM Node is back in sync", "nodeState", n.State())
	n.wg.Add(1)
	go n.aliveLoop()
}

func (n *node) transitionToInSync() (closed bool) {
	n.stateMu.Lock()
	defer n.stateMu.Unlock()
	switch n.state {
	case NodeStateClosed:
		return true
	case NodeStateOutOfSync:
		n.state = NodeStateAlive
	default:
		panic(fmt.Sprintf("cannot transition from %#v to NodeStateAlive", n.state))
	}
	return false
}

// declareOutOfSync puts a node into OutOfSync state, disconnecting all current
// clients and making it unavailable for use
func (n *node) declareOutOfSync(latestReceivedBlockNumber int64) {
	if closed := n.transitionToOutOfSync(); closed {
		return
	}
	n.log.Errorw("EVM Node is out of sync", "nodeState", n.State())
	n.wg.Add(1)
	go n.outOfSyncLoop(latestReceivedBlockNumber)
}

func (n *node) transitionToOutOfSync() (closed bool) {
	n.stateMu.Lock()
	defer n.stateMu.Unlock()
	switch n.state {
	case NodeStateClosed:
		return true
	case NodeStateAlive:
		// Need to disconnect all clients subscribed to this node
		n.ws.rpc.Close()
		n.cancel() // cancel all pending calls that didn't get killed by closing RPC above
		// Replace the context
		// NOTE: This is why all ctx access must happen inside the mutex
		n.ctx, n.cancel = context.WithCancel(context.Background())
		n.state = NodeStateOutOfSync
	default:
		panic(fmt.Sprintf("cannot transition from %#v to NodeStateOutOfSync", n.state))
	}
	return false
}

func (n *node) declareUnreachable() {
	if closed := n.transitionToUnreachable(); closed {
		// Closed: exit early
		return
	}
	n.log.Errorw("EVM Node is unreachable", "nodeState", n.State())
	n.wg.Add(1)
	go n.unreachableLoop()
}

func (n *node) transitionToUnreachable() (closed bool) {
	n.stateMu.Lock()
	defer n.stateMu.Unlock()
	switch n.state {
	case NodeStateClosed:
		return true
	case NodeStateDialed, NodeStateAlive, NodeStateOutOfSync:
		// Need to disconnect all clients subscribed to this node
		n.ws.rpc.Close()
		n.cancel() // cancel all pending calls that didn't get killed by closing RPC above
		// Replace the context
		// NOTE: This is why all ctx access must happen inside the mutex
		n.ctx, n.cancel = context.WithCancel(context.Background())
		n.state = NodeStateUnreachable
	default:
		panic(fmt.Sprintf("cannot transition from %#v to NodeStateUnreachable", n.state))
	}
	return false
}

func (n *node) declareInvalidChainID() {
	if closed := n.transitionToInvalidChainID(); closed {
		// Closed: exit early
		return
	}
	n.log.Errorw("EVM Node has the wrong chain ID", "nodeState", n.State())
	n.wg.Add(1)
	go n.invalidChainIDLoop()
}

func (n *node) transitionToInvalidChainID() (closed bool) {
	n.stateMu.Lock()
	defer n.stateMu.Unlock()
	switch n.state {
	case NodeStateClosed:
		return true
	case NodeStateDialed:
		// Need to disconnect all clients subscribed to this node
		n.ws.rpc.Close()
		n.cancel() // cancel all pending calls that didn't get killed by closing RPC above
		// Replace the context
		// NOTE: This is why all ctx access must happen inside the mutex
		n.ctx, n.cancel = context.WithCancel(context.Background())
		n.state = NodeStateInvalidChainID
	default:
		panic(fmt.Sprintf("cannot transition from %#v to NodeStateInvalidChainID", n.state))
	}
	return false
}
