package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/signal"
	"reflect"
	"sort"
	"syscall"
	"time"

	"github.com/cbergoon/merkletree"
	"github.com/gogo/status"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/andrew-delph/my-key-store/config"
	"github.com/andrew-delph/my-key-store/consensus"
	"github.com/andrew-delph/my-key-store/gossip"
	"github.com/andrew-delph/my-key-store/hashring"
	"github.com/andrew-delph/my-key-store/http"
	"github.com/andrew-delph/my-key-store/rpc"
	"github.com/andrew-delph/my-key-store/storage"
	"github.com/andrew-delph/my-key-store/utils"
)

type Manager struct {
	config                config.Config
	reqCh                 chan interface{}
	db                    storage.Storage
	httpServer            *http.HttpServer
	gossipCluster         *gossip.GossipCluster
	consensusCluster      *consensus.ConsensusCluster
	ring                  *hashring.Hashring
	rpcWrapper            *rpc.RpcWrapper
	myPartitions          *utils.IntSet
	partitionLocker       *PartitionLocker
	consistencyController *ConsistencyController
	debugTick             *time.Ticker
	CurrentEpoch          int64
}

func NewManager(c config.Config) Manager {
	reqCh := make(chan interface{}, c.Manager.ReqChannelSize)

	httpServer := http.CreateHttpServer(c.Http, reqCh)
	gossipCluster := gossip.CreateGossipCluster(c.Gossip, reqCh)
	db := storage.NewBadgerStorage(c.Storage)
	consensusCluster := consensus.CreateConsensusCluster(c.Consensus, reqCh)
	ring := hashring.CreateHashring(c.Manager)

	rpcWrapper := rpc.CreateRpcWrapper(c.Rpc, reqCh)
	parts := utils.NewIntSet()
	partitionLocker := NewPartitionLocker(c.Manager.PartitionCount)

	consistencyController := NewConsistencyController(c.Manager.PartitionConcurrency, c.Manager.PartitionCount, reqCh)
	return Manager{
		config:                c,
		reqCh:                 reqCh,
		db:                    db,
		httpServer:            &httpServer,
		gossipCluster:         gossipCluster,
		consensusCluster:      consensusCluster,
		ring:                  ring,
		rpcWrapper:            rpcWrapper,
		myPartitions:          &parts,
		partitionLocker:       partitionLocker,
		consistencyController: consistencyController,
		debugTick:             time.NewTicker(time.Second * 30),
	}
}

func (m *Manager) StartManager() {
	if m.config.Manager.PartitionBuckets%2 != 0 {
		logrus.Fatalf("PartitionBuckets must be even. PartitionBuckets = %d", m.config.Manager.PartitionBuckets)
	}
	var err error
	go m.startWorkers()

	go m.rpcWrapper.StartRpcServer()

	err = m.consensusCluster.StartConsensusCluster()
	if err != nil {
		logrus.Fatal(err)
	}
	err = m.gossipCluster.Join()
	if err != nil {
		logrus.Fatal(err)
	}

	go m.httpServer.StartHttp()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	<-signals
	logrus.Warn("Received SIGTERM signal")

	err = m.consensusCluster.Snapshot()
	if err != nil {
		logrus.Errorf("Failed to Snapshot err = %v", err)
	}
	err = m.httpServer.StopHttp()
	if err != nil {
		logrus.Errorf("Failed to Shutdown http server err = %v", err)
	}
}

func (m *Manager) startWorkers() {
	for i := 0; i < m.config.Manager.WokersCount; i++ {
		go m.startWorker(i)
	}
}

func (m *Manager) startWorker(workerId int) {
	logrus.Debugf("starting worker %d", workerId)
	defer logrus.Panicf("ending worker %d", workerId)

	for {
	workerLoop:
		select {
		case <-m.debugTick.C:
			// m.consensusCluster.Details()
			logrus.Debugf("DEBUG TICK #members = %d", len(m.gossipCluster.GetMembers()))

		case data, ok := <-m.reqCh:
			if !ok {
				logrus.Fatal("Channel closed!")
				return
			}
			switch task := data.(type) {

			case http.HealthTask:
				err := m.consensusCluster.IsHealthy()
				if err != nil {
					logrus.Warnf("HealthTask err = %v", err)
					task.ResCh <- err
				} else {
					logrus.Debug("HealthTask healthy")
					task.ResCh <- true
				}

			case http.SetTask:
				logrus.Debugf("worker SetTask: %+v", task)
				err := m.SetRequest(task.Key, task.Value)
				if err != nil {
					task.ResCh <- err
				} else {
					task.ResCh <- "value set"
				}

			case http.GetTask:
				logrus.Debugf("worker GetTask: %+v", task)
				value, err := m.GetRequest(task.Key)
				if err != nil {
					task.ResCh <- err
				} else if value == nil {
					task.ResCh <- nil
				} else {
					task.ResCh <- value.Value
				}

			case gossip.JoinTask:
				logrus.Warnf("worker JoinTask: %+v", task)

				err := m.consensusCluster.AddVoter(task.Name, task.IP)
				if err != nil {
					err = errors.Wrap(err, "gossip.JoinTask")
					logrus.Error(err)
				} else {
					// logrus.Infof("AddVoter success")
				}

				_, rpcClient, err := m.rpcWrapper.CreateRpcClient(task.IP)
				if err != nil {
					err = errors.Wrap(err, "gossip.JoinTask")
					logrus.Fatal(err)
					continue
				}

				m.ring.AddNode(CreateRingMember(task.Name, rpcClient))

				currPartitionsList, err := m.ring.GetMyPartions()
				if err != nil {
					logrus.Error(err)
					continue
				}
				currPartitions := utils.NewIntSet().From(currPartitionsList)
				m.consistencyController.HandleHashringChange(currPartitions)

			case gossip.LeaveTask:
				logrus.Warnf("worker LeaveTask: %+v", task)

				m.consensusCluster.RemoveServer(task.Name)

				m.ring.RemoveNode(task.Name)

				currPartitionsList, err := m.ring.GetMyPartions()
				if err != nil {
					logrus.Error(err)
					continue
				}
				currPartitions := utils.NewIntSet().From(currPartitionsList)
				m.consistencyController.HandleHashringChange(currPartitions)

			case consensus.EpochTask:
				m.CurrentEpoch = task.Epoch
				m.consistencyController.VerifyEpoch(task.Epoch)
				task.ResCh <- true

			case consensus.LeaderChangeTask:
				logrus.Warnf("worker LeaderChangeTask: %+v", task)

				if !task.IsLeader {
					continue
				}

				for _, member := range m.gossipCluster.GetMembers() {
					err := m.consensusCluster.AddVoter(member.Name, member.Addr.String())
					if err != nil {
						err = errors.Wrap(err, "gossip.JoinTask")
						logrus.Error(err)
						continue
					} else {
						logrus.Debugf("AddVoter success")
					}
				}

				if m.CurrentEpoch == 0 {
					err := m.consensusCluster.UpdateEpoch()
					if err != nil {
						logrus.Error("UpdateEpoch err = %v", err)
					}
				}

			case rpc.SetValueTask:
				logrus.Debugf("worker SetValueTask: %+v", task)
				err := m.SetValue(task.Value)
				if err != nil {
					task.ResCh <- err
				} else {
					task.ResCh <- true
				}

			case rpc.GetValueTask:
				logrus.Debugf("worker GetValueTask: %+v", task)
				// value, err := m.db.Get([]byte(task.Key))
				value, err := m.GetValue(task.Key)
				if err == storage.KEY_NOT_FOUND { // TODO if the nodes partition is not up to date it should not count as response
					task.ResCh <- nil
				} else if err != nil {
					task.ResCh <- err
				} else {
					task.ResCh <- value
				}

			case rpc.StreamBucketsTask: // TODO test this is returning right values
				logrus.Debugf("worker StreamBucketsTask: %+v", task)
				var buckets []int32 = task.Buckets
				if len(buckets) == 0 {
					for i := 0; i < m.config.Manager.PartitionBuckets; i++ {
						buckets = append(buckets, int32(i))
					}
				}
				for _, bucket := range buckets {
					index1, err := BuildEpochIndex(int(task.PartitionId), uint64(bucket), task.LowerEpoch, "")
					if err != nil {
						logrus.Fatal(err)
						continue
					}
					index2, err := BuildEpochIndex(int(task.PartitionId), uint64(bucket), task.UpperEpoch, "")
					if err != nil {
						logrus.Fatal(err)
						continue
					}
					it := m.db.NewIterator(
						[]byte(index1),
						[]byte(index2),
						false,
					)
					for !it.IsDone() {
						_, _, epoch, key, err := ParseEpochIndex(string(it.Key()))
						if err != nil {
							logrus.Fatal(err)
							continue
						}
						timestamp, err := utils.DecodeBytesToInt64(it.Value())
						if err != nil {
							logrus.Fatal(err)
							continue
						}

						task.ResCh <- &rpc.RpcValue{Key: key, Epoch: epoch, UnixTimestamp: timestamp}
						it.Next()
					}
					it.Release()
				}

				close(task.ResCh)

			case VerifyPartitionEpochRequestTask:
				logrus.Debugf("worker VerifyPartitionEpochRequestTask: %+v", task.PartitionId)
				err := m.VerifyEpoch(task.PartitionId, task.Epoch)

				if err != nil {
					task.ResCh <- err
				} else {
					task.ResCh <- VerifyPartitionEpochResponse{Valid: true}
				}

			case rpc.GetEpochTreeObjectTask:
				logrus.Debugf("worker GetPartitionEpochObjectTask: %+v", task)
				index, err := BuildEpochTreeObjectIndex(int(task.PartitionId), task.LowerEpoch)
				if err != nil {
					task.ResCh <- err
					continue
				}

				epochTreeObjectBytes, err := m.db.Get([]byte(index))
				if err != nil {
					task.ResCh <- err
					continue
				}

				epochTreeObject := &rpc.RpcEpochTreeObject{}
				err = proto.Unmarshal(epochTreeObjectBytes, epochTreeObject)
				if err != nil {
					task.ResCh <- err
					continue
				}
				task.ResCh <- epochTreeObject

			case rpc.GetEpochTreeLastValidObjectTask:
				logrus.Debugf("worker GetEpochTreeLastValidObjectTask: %+v", task)
				epochTreeObjectLastValid, err := m.GetEpochTreeLastValid(task.PartitionId)
				if err != nil {
					task.ResCh <- err
				} else if epochTreeObjectLastValid == nil {
					task.ResCh <- errors.New("no valid EpochTreeObject for partition")
				} else {
					task.ResCh <- epochTreeObjectLastValid
				}

			case SyncPartitionTask:
				logrus.Debugf("worker SyncPartitionTask: %+v", task.PartitionId)
				epochTreeObjectLastValid, err := m.GetEpochTreeLastValid(task.PartitionId)
				if err != nil {
					task.ResCh <- errors.Wrap(err, "GetEpochTreeLastValid")
					continue
				} else if epochTreeObjectLastValid != nil && epochTreeObjectLastValid.LowerEpoch >= task.UpperEpoch { // TODO validate this is the correct compare
					logrus.Warn("DOESNT NEED TO SYNC")
					task.ResCh <- SyncPartitionResponse{Valid: true}
					continue
				}

				lastValidEpoch := int64(0)
				if epochTreeObjectLastValid != nil {
					lastValidEpoch = epochTreeObjectLastValid.LowerEpoch
				}

				logrus.Warnf("sync lastValidEpoch %d", lastValidEpoch)

				// find most healthy node
				err = m.PoliteStreamRequest(int(task.PartitionId), lastValidEpoch, task.UpperEpoch+1, nil)

				if err != nil {
					logrus.Error(err)
				}

				logrus.Warnf("--- sync VerifyEpoch %d to %d", lastValidEpoch+1, task.UpperEpoch)

				for i := lastValidEpoch + 1; i <= task.UpperEpoch; i++ {
					err := m.VerifyEpoch(int(task.PartitionId), i)
					if err != nil {
						logrus.Fatalf("SyncPartitionTask VerifyEpoch PartitionId %d Epoch %d err = %v", task.PartitionId, i, err)
					} else {
						logrus.Warnf("[SYNC] VerifyEpoch PartitionId %d Epoch %d", task.PartitionId, i)
					}
				}

				task.ResCh <- SyncPartitionResponse{Valid: true}

				// stream from healthest node...

			default:
				logrus.Panicf("worker unkown task type: %v", reflect.TypeOf(task))
				break workerLoop
			}
		}
	}
}

func (m *Manager) SetRequest(key, value string) error {
	nodes, err := m.ring.GetClosestN(key, m.config.Manager.ReplicaCount, true)
	if err != nil {
		return err
	}

	unixTimestamp := time.Now().Unix()
	setReq := &rpc.RpcValue{Key: key, Value: value, Epoch: m.CurrentEpoch, UnixTimestamp: unixTimestamp}

	responseCh := make(chan *rpc.RpcStandardResponse, m.config.Manager.ReplicaCount)
	errorCh := make(chan error, m.config.Manager.ReplicaCount)

	for _, node := range nodes {
		member, ok := node.(RingMember)
		if !ok {
			return errors.New("failed to decode node")
		}

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res, err := member.rpcClient.SetRequest(ctx, setReq)
			if err != nil {
				errorCh <- errors.Wrapf(err, "member %s", member.Name)
			} else {
				responseCh <- res
			}
		}()
		// break
	}

	timeout := time.After(time.Second * time.Duration(m.config.Manager.DefaultTimeout))
	responseCount := 0

	for i := 0; i < m.config.Manager.ReplicaCount && responseCount < m.config.Manager.WriteQuorum; i++ {
		select {
		case <-responseCh:
			responseCount++
		case err := <-errorCh:
			logrus.Errorf("SetRequest errorCh: %v", err)
			_ = err // Handle error if necessary
		case <-timeout:
			return fmt.Errorf("timed out waiting for responses. responseCount = %d", responseCount)
		}
	}
	if responseCount < m.config.Manager.WriteQuorum {
		return fmt.Errorf("failed WriteQuorum. responseCount = %d", responseCount)
	} else {
		return nil
	}
}

func (m *Manager) GetRequest(key string) (*rpc.RpcValue, error) {
	nodes, err := m.ring.GetClosestN(key, m.config.Manager.ReplicaCount, true)
	if err != nil {
		return nil, err
	}

	getReq := &rpc.RpcGetRequestMessage{Key: key}
	responseCh := make(chan *rpc.RpcValue, m.config.Manager.ReplicaCount)
	errorCh := make(chan error, m.config.Manager.ReplicaCount)

	for _, node := range nodes {
		member, ok := node.(RingMember)
		if !ok {
			return nil, errors.New("failed to decode node")
		}

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res, err := member.rpcClient.GetRequest(ctx, getReq)
			st, ok := status.FromError(err)
			if ok && st.Code() == codes.NotFound {
				responseCh <- nil
			} else if err != nil {
				errorCh <- err
			} else {
				responseCh <- res
			}
		}()
	}

	responseCount := 0
	var recentValue *rpc.RpcValue
	timeout := time.After(time.Second * time.Duration(m.config.Manager.DefaultTimeout))
	for i := 0; i < m.config.Manager.ReplicaCount && responseCount < m.config.Manager.ReadQuorum; i++ {
		select {
		case res := <-responseCh:
			responseCount++

			if res == nil {
				// not found
				continue
			}

			if recentValue == nil {
				recentValue = res
			} else if recentValue.Epoch <= res.Epoch && recentValue.UnixTimestamp < res.UnixTimestamp {
				recentValue = res
			}
		case err := <-errorCh:
			logrus.Debugf("GetRequest errorCh %v", err)

		case <-timeout:
			return nil, fmt.Errorf("timed out waiting for responses. responseCount = %d", responseCount)
		}
	}
	if responseCount < m.config.Manager.ReadQuorum {
		return nil, fmt.Errorf("failed ReadQuorum. responseCount = %d", responseCount)
	} else if recentValue == nil {
		return nil, nil
	} else {
		return recentValue, nil
	}
}

func (m *Manager) EpochTreeObjectRequest(partitionId int, epoch int64, timeout time.Duration) ([]*rpc.RpcEpochTreeObject, error) {
	nodes, err := m.ring.GetClosestNForPartition(partitionId, m.config.Manager.ReplicaCount, true)
	if err != nil {
		return nil, err
	}

	treeReq := &rpc.RpcEpochTreeObject{Partition: int32(partitionId), LowerEpoch: epoch, UpperEpoch: epoch + 1}
	responseCh := make(chan *rpc.RpcEpochTreeObject, m.config.Manager.ReplicaCount)
	errorCh := make(chan error, m.config.Manager.ReplicaCount)

	for _, node := range nodes {
		member, ok := node.(RingMember)
		if !ok {
			return nil, errors.New("failed to decode node")
		}

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			res, err := member.rpcClient.GetEpochTree(ctx, treeReq)
			if err != nil {
				errorCh <- err
			} else {
				responseCh <- res
			}
		}()
	}

	var otherTrees []*rpc.RpcEpochTreeObject
	for i := 0; i < len(nodes); i++ {
		select {
		case res := <-responseCh:
			otherTrees = append(otherTrees, res)
		case err := <-errorCh:
			logrus.Debugf("GetEpochTree err = %v", err)
		}
	}
	return otherTrees, nil
}

func (m *Manager) SetValue(value *rpc.RpcValue) error {
	keyBytes := []byte(value.Key)
	timestampBytes, err := utils.EncodeInt64ToBytes(value.UnixTimestamp)
	if err != nil {
		return err
	}
	valueData, err := proto.Marshal(value)
	if err != nil {
		return err
	}
	partitionId := m.ring.FindPartitionID(keyBytes)
	hash := sha256.Sum256(keyBytes)
	bucket := binary.BigEndian.Uint64(hash[:8]) % uint64(m.config.Manager.PartitionBuckets)
	epochIndex, err := BuildEpochIndex(partitionId, bucket, value.Epoch, value.Key)
	if err != nil {
		return err
	}
	keyIndex, err := BuildKeyIndex(value.Key)
	if err != nil {
		return err
	}
	trx := m.db.NewTransaction(true)
	trx.Set([]byte(keyIndex), valueData)
	trx.Set([]byte(epochIndex), timestampBytes)
	return trx.Commit()
}

func (m *Manager) GetValue(key string) (*rpc.RpcValue, error) {
	keyIndex, err := BuildKeyIndex(key)
	if err != nil {
		return nil, err
	}

	valueBytes, err := m.db.Get([]byte(keyIndex))
	if err != nil {
		return nil, err
	}
	value := &rpc.RpcValue{}
	err = proto.Unmarshal(valueBytes, value)
	if err != nil {
		return nil, err
	}
	return value, nil
}

func (m *Manager) GetEpochTreeLastValid(partitionId int32) (*rpc.RpcEpochTreeObject, error) {
	index1, err := BuildEpochTreeObjectIndex(int(partitionId), 0)
	if err != nil {
		return nil, errors.Wrap(err, "BuildEpochTreeObjectIndex")
	}
	index2, err := BuildEpochTreeObjectIndex(int(partitionId), m.CurrentEpoch)
	if err != nil {
		return nil, errors.Wrap(err, "BuildEpochTreeObjectIndex")
	}
	it := m.db.NewIterator(
		[]byte(index1),
		[]byte(index2),
		true,
	)
	defer it.Release()
	for !it.IsDone() {
		epochTreeObjectBytes := it.Value()
		epochTreeObject := &rpc.RpcEpochTreeObject{}
		err = proto.Unmarshal(epochTreeObjectBytes, epochTreeObject)
		if err != nil {
			return nil, errors.WithMessagef(err, "RpcEpochTreeObject Unmarshal. epochTreeObjectBytes = %v", epochTreeObjectBytes)
		}

		if epochTreeObject.Valid {
			return epochTreeObject, nil
		}
		it.Next()
	}
	return nil, nil
}

type MemberEpochTreeLastValid struct {
	member             *RingMember
	epochTreeLastValid *rpc.RpcEpochTreeObject
}

func (m *Manager) EpochTreeLastValidRequest(partitionId int32, timeout time.Duration) ([]MemberEpochTreeLastValid, error) {
	nodes, err := m.ring.GetClosestNForPartition(int(partitionId), m.config.Manager.ReplicaCount, false)
	if err != nil {
		return nil, err
	}

	treeReq := &rpc.RpcEpochTreeObject{Partition: int32(partitionId)}
	responseCh := make(chan MemberEpochTreeLastValid, m.config.Manager.ReplicaCount)
	errorCh := make(chan error, m.config.Manager.ReplicaCount)

	for _, node := range nodes {
		member, ok := node.(RingMember)
		if !ok {
			return nil, errors.New("failed to decode node")
		}

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			res, err := member.rpcClient.GetEpochTreeLastValid(ctx, treeReq)
			if err != nil {
				errorCh <- err
			} else {
				responseCh <- MemberEpochTreeLastValid{member: &member, epochTreeLastValid: res}
			}
		}()
	}

	var membersLastValid []MemberEpochTreeLastValid
	for i := 0; i < len(nodes); i++ {
		select {
		case res := <-responseCh:
			membersLastValid = append(membersLastValid, res)
		case err := <-errorCh:
			logrus.Debugf("GetEpochTree err = %v", err)
		}
	}
	return membersLastValid, nil
}

func (m *Manager) SyncPartitionRequest(member *RingMember, partitionId int32, lowerEpoch int64, upperEpoch int64, buckets []int32, timeout time.Duration) error {
	logrus.Debugf("CLIENT SyncPartitionRequest")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req := &rpc.RpcStreamBucketsRequest{Partition: partitionId, LowerEpoch: lowerEpoch, UpperEpoch: upperEpoch, Buckets: buckets}
	streamClient, err := member.rpcClient.StreamBuckets(ctx, req)
	if err != nil {
		return errors.Wrap(err, "StreamBuckets request")
	}
	for {
		value, err := streamClient.Recv()
		if err == io.EOF {
			logrus.Debugf("CLIENT SyncPartitionRequest completed.")
			return nil
		} else if err != nil {
			return errors.Wrap(err, "StreamBuckets Recv")
		}

		// TODO change to get from epoch index.
		// if index value is less. write it.
		// then check the value. if the current value timestamp is greater then request from nodes
		keyBytes := []byte(value.Key)
		hash := sha256.Sum256(keyBytes)
		bucket := binary.BigEndian.Uint64(hash[:8]) % uint64(m.config.Manager.PartitionBuckets)
		epochIndex, err := BuildEpochIndex(int(partitionId), bucket, value.Epoch, value.Key)
		if err != nil {
			return err
		}

		epochBytes, err := m.db.Get([]byte(epochIndex))
		if epochBytes != nil {
			timestamp, err := utils.DecodeBytesToInt64(epochBytes)
			if err == nil && timestamp >= value.UnixTimestamp {
				logrus.Debugf("epochIndex ALREADY SYNCED~~~~~~~~~~~~~~~ KEY = %s", value.Key)
				continue
			}
		}

		myValue, err := m.GetValue(value.Key)
		if myValue != nil && myValue.UnixTimestamp >= value.UnixTimestamp {
			logrus.Debugf("GetValue ALREADY SYNCED!!!!!!!!!!!!!!!! KEY = %s", value.Key)
			continue
		} else {
			getReq := &rpc.RpcGetRequestMessage{Key: value.Key}
			syncedValue, err := member.rpcClient.GetRequest(ctx, getReq)
			if err != nil {
				logrus.Errorf("FAILED TO SYNC KEY = %s err = %v", value.Key, err)
				continue
			}
			if syncedValue == nil {
				logrus.Errorf("SYNC NOT FOUND KEY = %s err = %v", value.Key, err)
				continue
			}
			err = m.SetValue(syncedValue)
			if err != nil {
				logrus.Errorf("FAILED WRITE SYNC KEY = %s err = %v", syncedValue.Key, err)
				continue
			} else {
				logrus.Debugf("SYNC SUCCESS KEY = %s", syncedValue.Key)
			}
		}

		// verify each epoch from bottom up

		// If I have key and greater timestamp. ignore.
		// else request the key and write to my db.
	}
}

func (m *Manager) VerifyEpoch(PartitionId int, Epoch int64) error {
	// loop until successful verify
	var err error
	var myTree *merkletree.MerkleTree
	var otherTree *merkletree.MerkleTree
	var partitionEpochObject *rpc.RpcEpochTreeObject
	var index string
	var diff []int32
	var data []byte
	attempts := 0
	attemptsLimit := 2
	partitionLabel := fmt.Sprintf("%d", PartitionId)
	logrus.Debug("partitionLabel = ", partitionLabel)
	epochLabel := fmt.Sprintf("%d", Epoch)
	logrus.Debug("epochLabel = ", epochLabel)
	for {
		partitionVerifyEpochAttemptsGague.WithLabelValues(partitionLabel, epochLabel).Inc()
		if attempts > attemptsLimit {
			logrus.Errorf("VerifyEpoch: %v attempts= %d P= %d E= %d", err, attempts, PartitionId, Epoch)
		}
		if err != nil {
			time.Sleep(time.Second * 4)
		}
		attempts++

		myTree, err = m.RawPartitionMerkleTree(PartitionId, Epoch, Epoch+1)
		if err != nil {
			continue
		}
		// serialize
		partitionEpochObject, err = MerkleTreeToPartitionEpochObject(myTree, PartitionId, Epoch, Epoch+1)
		if err != nil {
			continue
		}
		data, err = proto.Marshal(partitionEpochObject)
		if err != nil {
			continue
		}

		// save to db
		index, err = BuildEpochTreeObjectIndex(PartitionId, Epoch)
		if err != nil {
			continue
		}
		err = m.db.Put([]byte(index), data)
		if err != nil {
			continue
		}

		var epochTreeObjects []*rpc.RpcEpochTreeObject

		epochTreeObjects, err = m.EpochTreeObjectRequest(PartitionId, Epoch, time.Second*20)
		if err != nil {
			continue
		}

		// if len(epochTreeObjects) < m.config.Manager.ReadQuorum {
		// 	err = errors.Errorf("need more trees #%d", len(epochTreeObjects))
		// 	continue
		// }
		// TODO do we need this?

		// compare the difference to the otherTree
		validCount := 0
		diffSet := utils.NewInt32Set()
		for _, epochTreeObject := range epochTreeObjects {
			otherTree, err = EpochTreeObjectToMerkleTree(epochTreeObject)
			if err != nil {
				logrus.Error(err)
				continue
			}
			diff, err = DifferentMerkleTreeBuckets(myTree, otherTree)
			if err != nil {
				logrus.Error(err)
				continue
			}
			diffSet.From(diff)

			if len(diff) == 0 {
				validCount++
			}
		}

		if validCount >= m.config.Manager.ReadQuorum {
			partitionEpochObject.Valid = true
			data, err = proto.Marshal(partitionEpochObject)
			if err != nil {
				continue
			}
			err = m.db.Put([]byte(index), data)
			if err != nil {
				continue
			}
			if attempts > attemptsLimit {
				logrus.Warnf("write partitionEpochObject Partition %v LowerEpoch %v index %s attempts %d", partitionEpochObject.Partition, partitionEpochObject.LowerEpoch, index, attempts)
			}

			partitionValidEpochGague.WithLabelValues(partitionLabel, epochLabel).Set(1)
			return nil
		} else {
			err = m.PoliteStreamRequest(int(partitionEpochObject.Partition), partitionEpochObject.LowerEpoch, partitionEpochObject.LowerEpoch+1, diffSet.List())
			err = errors.Errorf("validCount= %d against ReadQuorum %d", validCount, m.config.Manager.ReadQuorum)
		}
	}
}

func (m *Manager) PoliteStreamRequest(PartitionId int, LowerEpoch, UpperEpoch int64, buckets []int32) error {
	// find most healthy node
	// then stream from it
	membersLastValid, err := m.EpochTreeLastValidRequest(int32(PartitionId), time.Second*10)
	if err != nil {
		return err
	}

	sort.Slice(membersLastValid, func(i, j int) bool { // sort most healthy first
		return membersLastValid[i].epochTreeLastValid.LowerEpoch > membersLastValid[j].epochTreeLastValid.LowerEpoch
	})

	if len(membersLastValid) == 0 {
		return errors.New("membersLastValid is 0")
	}

	logrus.Debugf("sort f:%d %s l:%d %s #%d", membersLastValid[0].epochTreeLastValid.LowerEpoch, membersLastValid[0].member.Name, membersLastValid[len(membersLastValid)-1].epochTreeLastValid.LowerEpoch, membersLastValid[len(membersLastValid)-1].member.Name, len(membersLastValid))

	for _, lastValid := range membersLastValid {
		// logrus.Warnf("sync name %s lastValid %d", lastValid.member.Name, lastValid.epochTreeLastValid.LowerEpoch)
		err := m.SyncPartitionRequest(lastValid.member, int32(PartitionId), LowerEpoch, UpperEpoch, buckets, time.Second*60)
		if err != nil {
			logrus.Errorf("SyncPartitionRequest err = %v", err)
		} else {
			return nil
		}
	}

	return errors.New("PoliteStreamRequest errored on every member.")
}
