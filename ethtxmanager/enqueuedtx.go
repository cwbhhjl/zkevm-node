package ethtxmanager

import (
	"context"
	"math/big"
	"time"

	ethmanTypes "github.com/0xPolygonHermez/zkevm-node/etherman/types"
	"github.com/ethereum/go-ethereum/core/types"
)

type enqueuedTx interface {
	Tx() *types.Transaction
	RenewTxIfNeeded(context.Context, etherman) error
	Wait()
	WaitSync(ctx context.Context) error
}

type baseEnqueuedTx struct {
	tx           *types.Transaction
	waitDuration time.Duration
}

// Tx returns the internal tx
func (etx *baseEnqueuedTx) Tx() *types.Transaction {
	return etx.tx
}

// Tx returns the internal tx
func (etx *baseEnqueuedTx) Wait() {
	time.Sleep(etx.waitDuration)
}

// enqueuedSequencesTx represents a ethereum tx created to
// sequence batches that can be enqueued to be monitored
type enqueuedSequencesTx struct {
	baseEnqueuedTx
	state     state
	cfg       Config
	sequences []ethmanTypes.Sequence
}

// RenewTxIfNeeded checks for information in the inner tx and renews it
// if needed, for example changes the nonce is it realizes the nonce was
// already used or updates the gas price if the network has changed the
// prices since the tx was created
func (etx *enqueuedSequencesTx) RenewTxIfNeeded(ctx context.Context, e etherman) error {
	// nonce, err := e.CurrentNonce(ctx)
	// if err != nil {
	// 	return err
	// }
	// if etx.Tx().Nonce() < nonce {
	// 	err = etx.renewNonce(ctx, e)
	// 	if err != nil {
	// 		return err
	// 	}
	// }

	tx, err := e.EstimateGasSequenceBatches(etx.sequences)
	if err != nil {
		return err
	}
	if tx.Gas() > etx.Tx().Gas() {
		err = etx.renewGas(ctx, e)
		if err != nil {
			return err
		}
	}
	return nil
}

// // RenewNonce renews the inner TX nonce
// func (etx *enqueuedSequencesTx) renewNonce(ctx context.Context, e etherman) error {
// 	oldTx := etx.Tx()
// 	tx, err := e.SequenceBatches(ctx, etx.sequences, oldTx.Gas(), oldTx.GasPrice(), nil, true)
// 	if err != nil {
// 		return err
// 	}
// 	etx.baseEnqueuedTx.tx = tx
// 	return nil
// }

// RenewGasPrice renews the inner TX Gas Price
func (etx *enqueuedSequencesTx) renewGas(ctx context.Context, e etherman) error {
	oldTx := etx.Tx()
	oldNonce := big.NewInt(0).SetUint64(oldTx.Nonce())
	tx, err := e.SequenceBatches(ctx, etx.sequences, oldTx.Gas(), nil, oldNonce, true)
	if err != nil {
		return err
	}
	etx.baseEnqueuedTx.tx = tx
	return nil
}

// WaitSync checks if the sequences were already synced into the state
func (etx *enqueuedSequencesTx) WaitSync(ctx context.Context) error {
	return etx.state.WaitSequencingTxToBeSynced(ctx, etx.Tx(), etx.cfg.WaitTxToBeSynced.Duration)
}

// enqueuedVerifyBatchesTx represents a ethereum tx created to
// verify batches that can be enqueued to be monitored
type enqueuedVerifyBatchesTx struct {
	baseEnqueuedTx
	state             state
	cfg               Config
	lastVerifiedBatch uint64
	finalBatchNum     uint64
	inputs            *ethmanTypes.FinalProofInputs
}

// RenewTxIfNeeded checks for information in the inner tx and renews it
// if needed, for example changes the nonce is it realizes the nonce was
// already used or updates the gas price if the network has changed the
// prices since the tx was created
func (etx *enqueuedVerifyBatchesTx) RenewTxIfNeeded(ctx context.Context, e etherman) error {
	// nonce, err := e.CurrentNonce(ctx)
	// if err != nil {
	// 	return err
	// }
	// if etx.Tx().Nonce() < nonce {
	// 	err = etx.renewNonce(ctx, e)
	// 	if err != nil {
	// 		return err
	// 	}
	// }

	estimatedGas, err := e.EstimateGasForVerifyBatches(etx.lastVerifiedBatch, etx.finalBatchNum, etx.inputs)
	if err != nil {
		return err
	}
	if estimatedGas > etx.Tx().Gas() {
		err = etx.renewGas(ctx, e)
		if err != nil {
			return err
		}
	}
	return nil
}

// // RenewNonce renews the inner TX nonce
// func (etx *enqueuedVerifyBatchesTx) renewNonce(ctx context.Context, e etherman) error {
// 	oldTx := etx.Tx()
// 	tx, err := e.TrustedVerifyBatches(ctx, etx.lastVerifiedBatch, etx.finalBatchNum, etx.inputs, oldTx.Gas(), oldTx.GasPrice(), nil, true)
// 	if err != nil {
// 		return err
// 	}
// 	etx.baseEnqueuedTx.tx = tx
// 	return nil
// }

// RenewGasPrice renews the inner TX Gas Price
func (etx *enqueuedVerifyBatchesTx) renewGas(ctx context.Context, e etherman) error {
	oldTx := etx.Tx()
	oldNonce := big.NewInt(0).SetUint64(oldTx.Nonce())
	tx, err := e.VerifyBatches(ctx, etx.lastVerifiedBatch, etx.finalBatchNum, etx.inputs, oldTx.Gas(), nil, oldNonce, true)
	if err != nil {
		return err
	}
	etx.baseEnqueuedTx.tx = tx
	return nil
}

// WaitSync checks if the sequences were already synced into the state
func (etx *enqueuedVerifyBatchesTx) WaitSync(ctx context.Context) error {
	return etx.state.WaitVerifiedBatchToBeSynced(ctx, etx.finalBatchNum, etx.cfg.WaitTxToBeSynced.Duration)
}
