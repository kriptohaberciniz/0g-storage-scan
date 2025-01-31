package sync

import (
	"context"

	set "github.com/deckarep/golang-set"
	"github.com/ethereum/go-ethereum/common"
	"github.com/openweb3/go-rpc-provider"
	"github.com/openweb3/web3go"
	"github.com/openweb3/web3go/types"
	"github.com/pkg/errors"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrChainReorged = errors.New("chain re-orged")
)

type EthData struct {
	Number   uint64
	Block    *types.Block
	Receipts map[common.Hash]*types.Receipt
	Logs     []types.Log
}

func isTxExecutedInBlock(tx types.TransactionDetail, receipt types.Receipt) bool {
	return tx.BlockHash != nil && receipt.Status != nil && *receipt.Status < 2
}

func getEthDataByReceipts(w3c *web3go.Client, blockNumber uint64) (*EthData, error) {
	// get block
	block, err := w3c.Eth.BlockByNumber(types.BlockNumber(blockNumber), true)
	if err != nil {
		return nil, errors.WithMessagef(err, "failed to get block by number %v", blockNumber)
	}
	if block == nil {
		return nil, nil
	}

	// batch get receipts
	blockNumOrHash := types.BlockNumberOrHashWithNumber(types.BlockNumber(blockNumber))
	blockReceipts, err := w3c.Parity.BlockReceipts(&blockNumOrHash)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to get block receipts")
	}
	if blockReceipts == nil {
		return nil, errors.WithMessage(ErrChainReorged, "batch retrieved block receipts nil")
	}

	// get receipt
	txReceipts := map[common.Hash]*types.Receipt{}
	blockTxs := block.Transactions.Transactions()
	if len(blockTxs) != len(blockReceipts) {
		return nil, errors.Errorf("block receipts number mismatch, rcpts %v, txs %v", len(blockReceipts), len(blockTxs))
	}
	for i := 0; i < len(blockTxs); i++ {
		tx := blockTxs[i]
		receipt := &blockReceipts[i]

		// check re-org
		switch {
		case receipt.BlockHash != block.Hash:
			return nil, errors.WithMessagef(ErrChainReorged, "receipt block hash mismatch, rcptBlkHash %v, blkHash %v",
				receipt.BlockHash, block.Hash)
		case receipt.BlockNumber != blockNumber:
			return nil, errors.WithMessagef(ErrChainReorged, "receipt block num mismatch, rcptBlkNum %v, blkNum %v",
				receipt.BlockNumber, blockNumber)
		case receipt.TransactionHash != tx.Hash:
			return nil, errors.WithMessagef(ErrChainReorged, "receipt tx hash mismatch, rcptTxHash %v, TxHash %v",
				receipt.TransactionHash, tx.Hash)
		}
		txReceipts[tx.Hash] = receipt
	}

	return &EthData{Number: blockNumber, Block: block, Receipts: txReceipts}, nil
}

func getEthDataByLogs(w3c *web3go.Client, blockNumber uint64, addresses []common.Address, topics [][]common.Hash) (*EthData, error) {
	// get block
	block, err := w3c.Eth.BlockByNumber(types.BlockNumber(blockNumber), true)
	if err != nil {
		return nil, errors.WithMessagef(err, "failed to get block by number %v", blockNumber)
	}
	if block == nil {
		return nil, nil
	}

	// batch get logs
	//logArray, err := batchGetFlowSubmits(w3c, blockNumber, blockNumber, flowAddr, flowSubmitSig)
	logArray, err := batchGetLogs(w3c, blockNumber, blockNumber, addresses, topics)
	if err != nil {
		return nil, errors.WithMessagef(err, "failed to get flow submits in batch at block %v", blockNumber)
	}

	// check re-org
	txs := block.Transactions.Transactions()
	txIndex2Tx := make(map[uint64]types.TransactionDetail)
	for _, tx := range txs {
		txIndex2Tx[*tx.TransactionIndex] = tx
	}
	logs := make([]types.Log, 0)
	for i := 0; i < len(logArray); i++ {
		log := logArray[i]
		tx := txIndex2Tx[uint64(log.TxIndex)]
		switch {
		case log.BlockHash != block.Hash:
			return nil, errors.WithMessagef(ErrChainReorged, "log block hash mismatch, logBlkHash %v, blkHash %v",
				log.BlockHash, block.Hash)
		case log.BlockNumber != blockNumber:
			return nil, errors.WithMessagef(ErrChainReorged, "log block num mismatch, logBlkNum %v, blkNum %v",
				log.BlockNumber, blockNumber)
		case log.TxHash != tx.Hash:
			return nil, errors.WithMessagef(ErrChainReorged, "log tx hash mismatch, logTxHash %v, txHash %v",
				log.TxHash, tx.Hash)
		case uint64(log.TxIndex) != *tx.TransactionIndex:
			return nil, errors.WithMessagef(ErrChainReorged, "log tx index mismatch, logTxIndex %v, txIndex %v",
				log.TxIndex, tx.TransactionIndex)
		}
		logs = append(logs, log)
	}

	return &EthData{Number: blockNumber, Block: block, Logs: logs}, nil
}

func batchGetLogs(w3c *web3go.Client, blockFrom, blockTo uint64, addresses []common.Address,
	topics [][]common.Hash) ([]types.Log, error) {
	bnFrom := types.NewBlockNumber(int64(blockFrom))
	bnTo := types.NewBlockNumber(int64(blockTo))
	logFilter := types.FilterQuery{
		FromBlock: &bnFrom,
		ToBlock:   &bnTo,
		Addresses: addresses,
		Topics:    topics,
	}
	return w3c.Eth.Logs(logFilter)
}

func batchGetBlockTimes(ctx context.Context, w3c *web3go.Client, blkNums []types.BlockNumber,
	batchSize uint64) (map[uint64]uint64, error) {
	if len(blkNums) == 0 {
		return nil, errors.New("no block numbers")
	}

	blkNumSet := set.NewSet()
	for _, num := range blkNums {
		blkNumSet.Add(num)
	}

	blockNum2Time := make(map[uint64]uint64)
	blkNumSlice := blkNumSet.ToSlice()
	blkNumSize := len(blkNumSlice)
	for i := 0; i < blkNumSize; i += int(batchSize) {
		end := i + int(batchSize)
		if end > blkNumSize {
			end = blkNumSize
		}
		blockNums := blkNumSlice[i:end]

		batch := make([]rpc.BatchElem, 0)
		for _, blkNum := range blockNums {
			elem := rpc.BatchElem{
				Method: "eth_getBlockByNumber",
				Args:   []interface{}{blkNum, false},
				Result: new(types.Block),
			}
			batch = append(batch, elem)
		}

		err := w3c.Eth.BatchCallContext(ctx, batch)
		if err != nil {
			return nil, err
		}

		for _, elem := range batch {
			block := elem.Result.(*types.Block)
			blockNum2Time[block.Number.Uint64()] = block.Timestamp
		}
	}

	return blockNum2Time, nil
}
