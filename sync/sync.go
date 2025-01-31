package sync

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/0glabs/0g-storage-scan/store"
	viperUtil "github.com/Conflux-Chain/go-conflux-util/viper"
	"github.com/ethereum/go-ethereum/common"
	"github.com/openweb3/web3go"
	"github.com/openweb3/web3go/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type SyncConfig struct {
	BlockWhenFlowCreated     uint64
	DelayBlocksAgainstLatest uint64 `default:"30"`
	BatchBlocksOnCatchup     uint64 `default:"0"`
	BatchBlocksOnBatchCall   uint64 `default:"1000"`
	BatchTxsOnBatchCall      uint64 `default:"1000"`
}

type Syncer struct {
	conf                *SyncConfig
	sdk                 *web3go.Client
	db                  *store.MysqlStore
	currentBlock        uint64
	syncIntervalNormal  time.Duration
	syncIntervalCatchUp time.Duration
	catchupSyncer       *CatchupSyncer
	storageSyncer       *StorageSyncer
	flowAddr            string
	flowSubmitSig       string
	rewardAddr          string
	rewardSig           string
	addresses           []common.Address
	topics              [][]common.Hash
}

type storeData struct {
	submits []*store.Submit
	rewards []*store.Reward
}

// MustNewSyncer creates an instance of Syncer to sync blockchain data.
func MustNewSyncer(sdk *web3go.Client, db *store.MysqlStore, cf SyncConfig, cs *CatchupSyncer, ss *StorageSyncer) *Syncer {
	var flow struct {
		Address              string
		SubmitEventSignature string
	}
	viperUtil.MustUnmarshalKey("flow", &flow)

	var reward struct {
		Address              string
		RewardEventSignature string
	}
	viperUtil.MustUnmarshalKey("reward", &reward)

	syncer := &Syncer{
		conf:                &cf,
		sdk:                 sdk,
		db:                  db,
		syncIntervalNormal:  time.Second,
		syncIntervalCatchUp: time.Millisecond,
		catchupSyncer:       cs,
		storageSyncer:       ss,
		flowAddr:            flow.Address,
		flowSubmitSig:       flow.SubmitEventSignature,
		rewardAddr:          reward.Address,
		rewardSig:           reward.RewardEventSignature,
		addresses:           []common.Address{common.HexToAddress(flow.Address), common.HexToAddress(reward.Address)},
		topics:              [][]common.Hash{{common.HexToHash(flow.SubmitEventSignature), common.HexToHash(reward.RewardEventSignature)}},
	}

	// Load last sync block information
	syncer.mustLoadLastSyncBlock()

	return syncer
}

// Load last sync block from database to continue synchronization.
func (s *Syncer) mustLoadLastSyncBlock() {
	loaded, err := s.loadLastSyncBlock()
	if err != nil {
		logrus.WithError(err).Fatal("Failed to load last sync block from db")
	}

	// Load db sync start block config on initial loading if necessary.
	if !loaded && s.conf != nil {
		s.currentBlock = s.conf.BlockWhenFlowCreated
	}
}

func (s *Syncer) loadLastSyncBlock() (loaded bool, err error) {
	maxBlock, ok, err := s.db.MaxBlock()
	if err != nil {
		return false, errors.WithMessage(err, "failed to get max block from block table")
	}

	if ok {
		s.currentBlock = maxBlock + 1
	}

	return ok, nil
}

func (s *Syncer) Sync(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	defer wg.Done()

	err := s.checkReorgData()
	if err != nil {
		logrus.WithError(err).Error("Check reorg data error")
		return
	}

	s.catchupSyncer.Sync(ctx)
	s.currentBlock = s.catchupSyncer.finalizedBlock + 1
	logrus.WithField("block", s.catchupSyncer.finalizedBlock).Info("Catchup syncer done")

	go s.storageSyncer.Sync(ctx)

	ticker := time.NewTicker(s.syncIntervalCatchUp)
	defer ticker.Stop()

	logrus.Info("Syncer starting to sync data")
	for {
		select {
		case <-ctx.Done():
			logrus.Info("DB syncer shutdown ok")
			return
		case <-ticker.C:
			if err := s.doTicker(ticker); err != nil {
				logrus.WithError(err).
					WithField("currentBlock", s.currentBlock).
					Warn("Syncer failed to sync eth data")
			}
		}
	}
}

func (s *Syncer) doTicker(ticker *time.Ticker) error {
	logrus.Debug("Syncer ticking")

	complete, err := s.syncOnce()

	switch {
	case err != nil:
		ticker.Reset(s.syncIntervalNormal)
		return err
	case complete:
		ticker.Reset(s.syncIntervalNormal)
	default:
		ticker.Reset(s.syncIntervalCatchUp)
	}

	return nil
}

var (
	checkParityAPIAlready bool
	syncDataByLogs        bool
)

func (s *Syncer) syncOnce() (bool, error) {
	// get the latest block
	latestBlock, err := s.sdk.Eth.BlockNumber()
	if err != nil {
		return false, err
	}

	// check latest block
	curBlock := s.currentBlock
	if curBlock > latestBlock.Uint64()-s.conf.DelayBlocksAgainstLatest {
		return true, nil
	}

	// check parity api available
	if !checkParityAPIAlready {
		if _, err = getEthDataByReceipts(s.sdk, curBlock); err != nil && strings.Contains(err.Error(), "parity_getBlockReceipts") {
			syncDataByLogs = true
		}
		checkParityAPIAlready = true
	}

	// get eth data
	var data *EthData
	if syncDataByLogs {
		data, err = getEthDataByLogs(s.sdk, curBlock, s.addresses, s.topics)
	} else {
		data, err = getEthDataByReceipts(s.sdk, curBlock)
	}
	if err != nil {
		return false, err
	}
	if data == nil {
		return true, nil
	}

	// check pivot hash
	latestBlockHash, err := s.getStoreLatestBlockHash()
	if err != nil {
		return false, err
	}
	if len(latestBlockHash) > 0 && data.Block.ParentHash.Hex() != latestBlockHash {
		return false, s.revertReorgData(s.latestStoreBlock())
	}

	// persist db
	block := store.NewBlock(data.Block)
	sd, err := s.parseEthData(data)
	if err != nil {
		return false, err
	}
	if err = s.db.Push(block, sd.submits, sd.rewards); err != nil {
		return false, err
	}

	// increase currentBlock
	if s.currentBlock%100 == 0 {
		logrus.WithField("block", s.currentBlock).Info("Sync data")
	}
	s.currentBlock++

	return false, nil
}

func (s *Syncer) getStoreLatestBlockHash() (string, error) {
	blockFlowContractCreated := s.conf.BlockWhenFlowCreated
	if s.currentBlock <= blockFlowContractCreated {
		return "", nil
	}

	latestBlockNo := s.latestStoreBlock()
	hash, _, err := s.db.BlockHash(latestBlockNo)
	return hash, err
}

func (s *Syncer) latestStoreBlock() uint64 {
	if s.currentBlock > 0 {
		return s.currentBlock - 1
	}

	return 0
}

// Ideally, one by one block to check and prune reorg data is fine though, this could be improved with binary search probing.
func (s *Syncer) checkReorgData() error {
	for {
		blockNum, ok, err := s.db.MaxBlock()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}

		exist, err := s.existReorgData(blockNum)
		if err != nil {
			return err
		}

		if !exist {
			break
		}

		err = s.revertReorgData(blockNum)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Syncer) existReorgData(blockNum uint64) (bool, error) {
	hash, ok, err := s.db.BlockHash(blockNum)
	if err != nil {
		return false, errors.WithMessagef(err, "failed to get block at %v from db", blockNum)
	}
	if !ok {
		return false, nil
	}

	block, err := s.sdk.Eth.BlockByNumber(types.BlockNumber(blockNum), false)
	if err != nil {
		return false, errors.WithMessagef(err, "failed to get block at %v from blockchain", blockNum)
	}

	return hash != block.Hash.String(), nil
}

func (s *Syncer) revertReorgData(revertBlock uint64) error {
	// pop from db
	logrus.WithField("block", revertBlock).Info("Revert eth data")
	if err := s.db.Pop(revertBlock); err != nil {
		return errors.WithMessage(err, "failed to pop eth data from db")
	}

	// update currentBlock
	s.currentBlock = revertBlock

	return nil
}

func (s *Syncer) parseEthData(data *EthData) (*storeData, error) {
	var logs []types.Log
	if syncDataByLogs {
		logs = data.Logs
	} else {
		for _, t := range data.Block.Transactions.Transactions() {
			rcpt := data.Receipts[t.Hash]
			if rcpt == nil || !isTxExecutedInBlock(t, *rcpt) {
				continue
			}
			for _, log := range rcpt.Logs {
				logs = append(logs, *log)
			}
		}
	}

	bn2TimeMap := map[uint64]uint64{data.Block.Number.Uint64(): data.Block.Timestamp}

	submits, rewards, err := s.catchupSyncer.convertLogs(logs, bn2TimeMap)
	if err != nil {
		return nil, err
	}

	return &storeData{submits, rewards}, nil
}
