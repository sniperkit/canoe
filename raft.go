package raftwrapper

import (
	"fmt"
	"github.com/coreos/etcd/etcdserver/stats"
	"github.com/coreos/etcd/pkg/types"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/coreos/etcd/rafthttp"
	"golang.org/x/net/context"
	"strconv"
	"sync"
	"time"
)

type LogData []byte

type Node struct {
	node        raft.Node
	raftStorage *raft.MemoryStorage
	transport   *rafthttp.Transport
	peers       []string
	peerMap     map[uint64]string
	id          uint64
	raftPort    int
	cluster     int

	apiPort int

	started     bool
	initialized bool
	proposeC    chan string
	fsm         FSM

	observers     map[uint64]*Observer
	observersLock sync.RWMutex
}

type NodeConfig struct {
	FSM           FSM
	RaftPort      int
	APIPort       int
	Peers         []string
	BootstrapNode bool
}

// note: peers is only for asking to join the cluster.
// It will not be able to connect if the peers don't respond to cluster node add request
// This is because each node defines it's own uuid at startup. We must be told this UUID
// by another node.
// TODO: Look into which config options we want others to specify. For now hardcoded
// NOTE: Peers are used EXCLUSIVELY to round-robin to other nodes and attempt to add
//		ourselves to an existing cluster or bootstrap node
func NewNode(args *NodeConfig) (*Node, error) {
	rn := nonInitNode(args)

	if err := rn.attachTransport(); err != nil {
		return nil, err
	}

	return rn, nil
}

func (rn *Node) Start(httpBlock bool) error {
	if rn.started {
		return nil
	}

	var wg sync.WaitGroup

	if err := rn.transport.Start(); err != nil {
		return err
	}

	go rn.scanReady()

	if httpBlock {
		wg.Add(1)
	}
	go func(rn *Node) {
		defer wg.Done()
		rn.serveHTTP()
	}(rn)

	go rn.serveRaft()

	if err := rn.joinPeers(); err != nil {
		return err
	}
	// final step to mark node as initialized
	// TODO: Find a better place to mark initialized
	rn.initialized = true
	rn.started = true
	wg.Wait()
	return nil
}

func nonInitNode(args *NodeConfig) *Node {
	if args.BootstrapNode {
		args.Peers = nil
	}
	rn := &Node{
		proposeC:    make(chan string),
		cluster:     0x1000,
		raftStorage: raft.NewMemoryStorage(),
		peers:       args.Peers,
		id:          Uint64UUID(),
		raftPort:    args.RaftPort,
		apiPort:     args.APIPort,
		fsm:         args.FSM,
		initialized: false,
		observers:   make(map[uint64]*Observer),
		peerMap:     make(map[uint64]string),
	}

	c := &raft.Config{
		ID:              rn.id,
		ElectionTick:    10,
		HeartbeatTick:   1,
		Storage:         rn.raftStorage,
		MaxSizePerMsg:   1024 * 1024,
		MaxInflightMsgs: 256,
	}

	if args.BootstrapNode {
		rn.node = raft.StartNode(c, []raft.Peer{raft.Peer{ID: rn.id}})
	} else {
		rn.node = raft.StartNode(c, nil)
	}

	return rn
}

func (rn *Node) attachTransport() error {
	ss := &stats.ServerStats{}
	ss.Initialize()

	rn.transport = &rafthttp.Transport{
		ID:          types.ID(rn.id),
		ClusterID:   0x1000,
		Raft:        rn,
		ServerStats: ss,
		LeaderStats: stats.NewLeaderStats(strconv.FormatUint(rn.id, 10)),
		ErrorC:      make(chan error),
	}

	return nil
}

func (rn *Node) joinPeers() error {
	err := rn.requestSelfAddition()
	if err != nil {
		return err
	}
	return nil
}

func (rn *Node) proposePeerAddition(addReq *raftpb.ConfChange, async bool) error {
	addReq.Type = raftpb.ConfChangeAddNode

	observChan := make(chan Observation)
	// setup listener for node addition
	// before asking for node addition
	if !async {
		filterFn := func(o Observation) bool {

			switch o.(type) {
			case raftpb.Entry:
				entry := o.(raftpb.Entry)
				switch entry.Type {
				case raftpb.EntryConfChange:
					var cc raftpb.ConfChange
					cc.Unmarshal(entry.Data)
					rn.node.ApplyConfChange(cc)
					switch cc.Type {
					case raftpb.ConfChangeAddNode:
						// wait until we get a matching node id
						return addReq.NodeID == cc.NodeID
					default:
						return false
					}
				default:
					return false
				}
			default:
				return false
			}
		}

		observer := NewObserver(observChan, filterFn)
		rn.RegisterObserver(observer)
		defer rn.UnregisterObserver(observer)
	}

	if err := rn.node.ProposeConfChange(context.TODO(), *addReq); err != nil {
		return err
	}

	if async {
		return nil
	}

	// TODO: Do a retry here on failure for x retries
	select {
	case <-observChan:
		return nil
	case <-time.After(10 * time.Second):
		return rn.proposePeerAddition(addReq, async)

	}
}

func (rn *Node) canAddPeer() bool {
	return rn.isHealthy() && rn.initialized
}

// TODO: Define healthy
func (rn *Node) isHealthy() bool {
	return true
}

func (rn *Node) scanReady() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rn.node.Tick()
		case rd := <-rn.node.Ready():
			rn.raftStorage.Append(rd.Entries)
			rn.transport.Send(rd.Messages)
			if ok := rn.publishEntries(rd.CommittedEntries); !ok {
				return
			}

			rn.node.Advance()
		}
	}
}

func (rn *Node) publishEntries(ents []raftpb.Entry) bool {
	for _, entry := range ents {
		switch entry.Type {
		case raftpb.EntryNormal:
			if len(entry.Data) == 0 {
				break
			}
			// Yes, this is probably a blocking call
			// An FSM should be responsible for being efficient
			// for high-load situations
			if err := rn.fsm.Apply(LogData(entry.Data)); err != nil {
				return false
			}

		case raftpb.EntryConfChange:
			var cc raftpb.ConfChange
			cc.Unmarshal(entry.Data)
			rn.node.ApplyConfChange(cc)

			switch cc.Type {
			case raftpb.ConfChangeAddNode:
				if len(cc.Context) > 0 {
					rn.transport.AddPeer(types.ID(cc.NodeID), []string{string(cc.Context)})
					rn.peerMap[cc.NodeID] = string(cc.Context)
				}
			case raftpb.ConfChangeRemoveNode:
				if cc.NodeID == uint64(rn.id) {
					fmt.Println("I have been removed!")
					return false
				}
				rn.transport.RemovePeer(types.ID(cc.NodeID))
			}

		}
		rn.observe(entry)
		// TODO: Add support for replay commits
		// After replaying old commits/snapshots then mark
		// this node operational
	}
	return true
}

func (rn *Node) Propose(data []byte) error {
	return rn.node.Propose(context.TODO(), data)
}

func (rn *Node) Process(ctx context.Context, m raftpb.Message) error {
	return rn.node.Step(ctx, m)
}
func (rn *Node) IsIDRemoved(id uint64) bool {
	return false
}
func (rn *Node) ReportUnreachable(id uint64)                          {}
func (rn *Node) ReportSnapshot(id uint64, status raft.SnapshotStatus) {}