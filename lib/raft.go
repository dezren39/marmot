package lib

import (
    "context"
    "fmt"
    "strconv"
    "strings"
    "sync"
    "time"

    "github.com/lni/dragonboat/v3"
    "github.com/lni/dragonboat/v3/config"
    "github.com/lni/dragonboat/v3/logger"
    "github.com/lni/dragonboat/v3/raftio"
    "github.com/lni/dragonboat/v3/statemachine"
    "github.com/rs/zerolog/log"
    "marmot/db"
)

type RaftServer struct {
    bindAddress  string
    nodeID       uint64
    metaPath     string
    lock         *sync.RWMutex
    clusterMap   map[uint64]uint64
    stateMachine statemachine.IStateMachine
    nodeHost     *dragonboat.NodeHost
}

func NewRaftServer(
    bindAddress string,
    nodeID uint64,
    metaPath string,
    database *db.SqliteStreamDB,
) *RaftServer {
    logger.GetLogger("raft").SetLevel(logger.ERROR)
    logger.GetLogger("rsm").SetLevel(logger.WARNING)
    logger.GetLogger("transport").SetLevel(logger.ERROR)
    logger.GetLogger("grpc").SetLevel(logger.WARNING)

    return &RaftServer{
        bindAddress:  bindAddress,
        nodeID:       nodeID,
        metaPath:     metaPath,
        clusterMap:   make(map[uint64]uint64),
        lock:         &sync.RWMutex{},
        stateMachine: db.NewDBStateMachine(nodeID, database),
    }
}

func (r *RaftServer) config(clusterID uint64) config.Config {
    return config.Config{
        NodeID:                  r.nodeID,
        ClusterID:               clusterID,
        ElectionRTT:             10,
        HeartbeatRTT:            1,
        CheckQuorum:             true,
        SnapshotEntries:         100_000,
        CompactionOverhead:      1,
        EntryCompressionType:    config.Snappy,
        SnapshotCompressionType: config.Snappy,
    }
}

func (r *RaftServer) LeaderUpdated(info raftio.LeaderInfo) {
    r.lock.Lock()
    defer r.lock.Unlock()

    if info.LeaderID == 0 {
        delete(r.clusterMap, info.ClusterID)
    } else {
        r.clusterMap[info.ClusterID] = info.LeaderID
    }

    log.Info().Msg(fmt.Sprintf("Leader updated... %v -> %v", info.ClusterID, info.LeaderID))
}

func (r *RaftServer) Init() error {
    r.lock.Lock()
    defer r.lock.Unlock()

    metaAbsPath := fmt.Sprintf("%s/node-%d", r.metaPath, r.nodeID)
    hostConfig := config.NodeHostConfig{
        WALDir:            metaAbsPath,
        NodeHostDir:       metaAbsPath,
        RTTMillisecond:    300,
        RaftAddress:       r.bindAddress,
        RaftEventListener: r,
    }

    nodeHost, err := dragonboat.NewNodeHost(hostConfig)
    if err != nil {
        return err
    }

    r.nodeHost = nodeHost
    return nil
}

func (r *RaftServer) BindCluster(initMembers string, join bool, clusterIDs ...uint64) error {
    initialMembers := parseInitialMembersMap(initMembers)
    if !join {
        initialMembers[r.nodeID] = r.bindAddress
    }

    r.lock.Lock()
    defer r.lock.Unlock()

    for _, clusterID := range clusterIDs {
        log.Debug().Uint64("cluster", clusterID).Msg("Starting cluster...")
        cfg := r.config(clusterID)
        err := r.nodeHost.StartCluster(initialMembers, join, r.stateMachineFactory, cfg)
        if err != nil {
            return err
        }

        r.clusterMap[clusterID] = 0
    }

    return nil
}

func (r *RaftServer) AddNode(peerID uint64, address string, clusterIDs ...uint64) error {
    r.lock.Lock()
    defer r.lock.Unlock()

    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()

    for _, clusterID := range clusterIDs {
        mem, err := r.nodeHost.SyncGetClusterMembership(ctx, clusterID)
        if err != nil {
            return err
        }

        err = r.nodeHost.SyncRequestAddNode(ctx, clusterID, peerID, address, mem.ConfigChangeID)
        if err != nil {
            return err
        }
    }

    return nil
}

func (r *RaftServer) TransferClusters(toPeerID uint64, clusterIDs ...uint64) error {
    for _, cluster := range clusterIDs {
        err := r.nodeHost.RequestLeaderTransfer(cluster, toPeerID)
        if err != nil {
            return err
        }
    }

    return nil
}

func (r *RaftServer) GetActiveClusters() []uint64 {
    r.lock.RLock()
    defer r.lock.RUnlock()

    ret := make([]uint64, 0)
    for clusterID := range r.clusterMap {
        ret = append(ret, clusterID)
    }

    return ret
}

func (r *RaftServer) GetClusterMap() map[uint64]uint64 {
    r.lock.RLock()
    defer r.lock.RUnlock()
    return r.clusterMap
}

func (r *RaftServer) Propose(key uint64, data []byte, dur time.Duration) (*dragonboat.RequestResult, error) {
    clusterIds := r.GetActiveClusters()
    clusterIndex := uint64(1)
    if len(clusterIds) != 0 {
        clusterIndex = key % uint64(len(clusterIds))
    }

    session := r.nodeHost.GetNoOPSession(clusterIds[clusterIndex])
    req, err := r.nodeHost.Propose(session, data, dur)
    if err != nil {
        return nil, err
    }

    res := <-req.ResultC()
    return &res, err
}

func (r *RaftServer) stateMachineFactory(_ uint64, _ uint64) statemachine.IStateMachine {
    return r.stateMachine
}

func parseInitialMembersMap(peersAddrs string) map[uint64]string {
    peersMap := make(map[uint64]string)
    if peersAddrs == "" {
        return peersMap
    }

    for _, peer := range strings.Split(peersAddrs, ",") {
        peerInf := strings.Split(peer, "@")
        peerShard, err := strconv.ParseUint(peerInf[0], 10, 64)
        if err != nil {
            continue
        }

        peersMap[peerShard] = peerInf[1]
    }

    return peersMap
}