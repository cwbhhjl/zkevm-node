package ethtxmanager

import (
	"encoding/hex"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	// MonitoredTxStatusCreated mean the tx was just added to the storage
	MonitoredTxStatusCreated = MonitoredTxStatus("created")

	// MonitoredTxStatusSent means that at least a eth tx was sent to the network
	MonitoredTxStatusSent = MonitoredTxStatus("sent")

	// MonitoredTxStatusFailed means the tx was already mined and failed with an
	// error that can be recovered automatically, ex: the data in the tx is invalid
	// and the tx gets reverted
	MonitoredTxStatusFailed = MonitoredTxStatus("failed")

	// MonitoredTxStatusConfirmed means the tx was already mined and the receipt
	// status is Successful
	MonitoredTxStatusConfirmed = MonitoredTxStatus("confirmed")
)

// MonitoredTxStatus represents the status of a monitored tx
type MonitoredTxStatus string

// monitoredTx represents a set of information used to build tx
// plus information to monitor if the transactions was sent successfully
type monitoredTx struct {
	// id is the tx identifier controller by the caller
	id string

	// sender of the tx, used to identify which private key should be used to sing the tx
	from common.Address

	// receiver of the tx
	to *common.Address

	// nonce used to create the tx
	nonce uint64

	// tx value
	value *big.Int

	// tx data
	data []byte

	// tx gas
	gas uint64

	// tx gas price
	gasPrice *big.Int

	// status of this monitoring
	status MonitoredTxStatus

	// history represent all transaction hashes from
	// transactions created using this struct data and
	// sent to the network
	history map[common.Hash]bool

	// createdAt date time it was created
	createdAt time.Time

	// updatedAt last date time it was updated
	updatedAt time.Time
}

// Tx uses the current information to build a tx
func (mTx monitoredTx) Tx() *types.Transaction {
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    mTx.nonce,
		Value:    mTx.value,
		Data:     mTx.data,
		Gas:      mTx.gas,
		GasPrice: mTx.gasPrice,
	})

	return tx
}

// AddHistory adds a transaction to the monitoring history
func (mTx monitoredTx) AddHistory(tx *types.Transaction) error {
	if _, found := mTx.history[tx.Hash()]; found {
		return ErrAlreadyExists
	}
	mTx.history[tx.Hash()] = true
	return nil
}

// toStringPtr returns the current to field as a string pointer
func (mTx *monitoredTx) toStringPtr() *string {
	var to *string
	if mTx.to != nil {
		s := mTx.to.String()
		to = &s
	}
	return to
}

// valueU64Ptr returns the current value field as a uint64 pointer
func (mTx *monitoredTx) valueU64Ptr() *uint64 {
	var value *uint64
	if mTx.value != nil {
		tmp := mTx.value.Uint64()
		value = &tmp
	}
	return value
}

// dataStringPtr returns the current data field as a string pointer
func (mTx *monitoredTx) dataStringPtr() *string {
	var data *string
	if mTx.data != nil {
		tmp := hex.EncodeToString(mTx.data)
		data = &tmp
	}
	return data
}

// historyStringSlice returns the current history field as a string slice
func (mTx *monitoredTx) historyStringSlice() []string {
	history := make([]string, 0, len(mTx.history))
	for h := range mTx.history {
		history = append(history, h.String())
	}
	return history
}
