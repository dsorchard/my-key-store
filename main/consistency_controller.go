package main

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/pkg/errors"
	"github.com/reactivex/rxgo/v2"
	"github.com/sirupsen/logrus"

	"github.com/andrew-delph/my-key-store/utils"
)

type PartitionLocker struct {
	partitionLockMap sync.Map
}

func NewPartitionLocker(partitionCount int) *PartitionLocker {
	pl := &PartitionLocker{}
	for i := 0; i < partitionCount; i++ {
		pl.partitionLockMap.Store(i, false)
	}
	return pl
}

func (pl *PartitionLocker) Lock(partition int) error {
	swapped := pl.partitionLockMap.CompareAndSwap(partition, false, true)
	if !swapped {
		return errors.New("could not swap key")
	}
	return nil
}

func (pl *PartitionLocker) Unlock(partition int) error {
	pl.partitionLockMap.Store(partition, false)
	return nil
}

type VerifyPartitionEpochRequestTask struct {
	PartitionId int
	Epoch       int64
	ResCh       chan interface{}
}

type VerifyPartitionEpochResponse struct {
	Valid bool
}

type SyncPartitionTask struct {
	PartitionId int
	ResCh       chan interface{}
}

type SyncPartitionResponse struct {
	Valid bool
}

type UpdatePartitionsEvent struct {
	CurrPartitions utils.IntSet
}

type VerifyPartitionEpochEvent struct {
	Epoch int64
}

type ConsistencyController struct {
	observationCh    chan rxgo.Item
	partitionsStates []*PartitionState
	observable       rxgo.Observable
}

func NewConsistencyController(partitionCount int, reqCh chan interface{}) *ConsistencyController {
	// TODO create semaphore.NewWeighted(int64(limit)) for number of partition observer events at once
	ch := make(chan rxgo.Item)
	observable := rxgo.FromChannel(ch, rxgo.WithPublishStrategy())
	var partitionsStates []*PartitionState
	for i := 0; i < partitionCount; i++ {
		partitionsStates = append(partitionsStates, NewPartitionState(i, observable, reqCh))
	}
	observable.Connect(context.Background())
	return &ConsistencyController{observationCh: ch, partitionsStates: partitionsStates, observable: observable}
}

func (cc *ConsistencyController) HandleHashringChange(currPartitions utils.IntSet) error {
	cc.PublishEvent(UpdatePartitionsEvent{CurrPartitions: currPartitions})
	return nil
}

func (cc *ConsistencyController) VerifyEpoch(Epoch int64) {
	cc.PublishEvent(VerifyPartitionEpochEvent{Epoch: Epoch})
}

func (cc *ConsistencyController) PublishEvent(event interface{}) {
	cc.observationCh <- rxgo.Of(event)
}

type PartitionState struct {
	partitionId int
	active      atomic.Bool
}

func NewPartitionState(partitionId int, observable rxgo.Observable, reqCh chan interface{}) *PartitionState {
	ps := &PartitionState{partitionId: partitionId}
	observable.DoOnNext(func(item interface{}) {
		switch event := item.(type) {
		case VerifyPartitionEpochEvent: // TODO create test case for this
			if ps.active.Load() {
				resCh := make(chan interface{})
				logrus.Debugf("trigger verify epoch event. partition %d epoch %d", partitionId, event.Epoch)
				reqCh <- VerifyPartitionEpochRequestTask{PartitionId: partitionId, Epoch: event.Epoch, ResCh: resCh}
				rawRes := <-resCh
				switch res := rawRes.(type) {
				case VerifyPartitionEpochResponse:
					logrus.Debugf("verify epoch=%d partitionId=%d res = %+v", event.Epoch, partitionId, res)
				default:
					logrus.Panicf("VerifyPartitionEpochEvent observer unkown res type: %v", reflect.TypeOf(res))
				}
			}

		case UpdatePartitionsEvent: // TODO create test case for this
			if event.CurrPartitions.Has(partitionId) && ps.active.CompareAndSwap(false, true) { // TODO create test case for this
				logrus.Warnf("trigger new partition sync %d", partitionId)
				resCh := make(chan interface{})
				reqCh <- SyncPartitionTask{PartitionId: partitionId, ResCh: resCh}
				rawRes := <-resCh
				switch res := rawRes.(type) {
				case SyncPartitionResponse:
					logrus.Warnf("sync partrition %d res = %+v", partitionId, res)
				default:
					logrus.Panicf("UpdatePartitionsEvent observer unkown res type: %v", reflect.TypeOf(res))
				}
			} else if !event.CurrPartitions.Has(partitionId) && ps.active.CompareAndSwap(true, false) {
				logrus.Warnf("updated lost partition active %d", partitionId)
			}
		default:
			logrus.Warn("unknown PartitionState event")
		}
	})
	return ps
}