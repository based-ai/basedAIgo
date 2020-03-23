// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package avm

import (
	"errors"

	"github.com/ava-labs/gecko/database"
	"github.com/ava-labs/gecko/ids"
	"github.com/ava-labs/gecko/snow"
	"github.com/ava-labs/gecko/vms/components/ava"
	"github.com/ava-labs/gecko/vms/components/codec"
	"github.com/ava-labs/gecko/vms/components/verify"
)

var (
	errNilTx          = errors.New("nil tx is not valid")
	errWrongNetworkID = errors.New("tx has wrong network ID")
	errWrongChainID   = errors.New("tx has wrong chain ID")

	errOutputsNotSorted      = errors.New("outputs not sorted")
	errInputsNotSortedUnique = errors.New("inputs not sorted and unique")

	errInputOverflow     = errors.New("inputs overflowed uint64")
	errOutputOverflow    = errors.New("outputs overflowed uint64")
	errInsufficientFunds = errors.New("insufficient funds")
)

// BaseTx is the basis of all transactions.
type BaseTx struct {
	ava.Metadata

	NetID uint32                `serialize:"true"` // ID of the network this chain lives on
	BCID  ids.ID                `serialize:"true"` // ID of the chain on which this transaction exists (prevents replay attacks)
	Outs  []*TransferableOutput `serialize:"true"` // The outputs of this transaction
	Ins   []*TransferableInput  `serialize:"true"` // The inputs to this transaction
}

// NetworkID is the ID of the network on which this transaction exists
func (t *BaseTx) NetworkID() uint32 { return t.NetID }

// ChainID is the ID of the chain on which this transaction exists
func (t *BaseTx) ChainID() ids.ID { return t.BCID }

// Outputs track which outputs this transaction is producing. The returned array
// should not be modified.
func (t *BaseTx) Outputs() []*TransferableOutput { return t.Outs }

// Inputs track which UTXOs this transaction is consuming. The returned array
// should not be modified.
func (t *BaseTx) Inputs() []*TransferableInput { return t.Ins }

// InputUTXOs track which UTXOs this transaction is consuming.
func (t *BaseTx) InputUTXOs() []*ava.UTXOID {
	utxos := []*ava.UTXOID(nil)
	for _, in := range t.Ins {
		utxos = append(utxos, &in.UTXOID)
	}
	return utxos
}

// AssetIDs returns the IDs of the assets this transaction depends on
func (t *BaseTx) AssetIDs() ids.Set {
	assets := ids.Set{}
	for _, in := range t.Ins {
		assets.Add(in.AssetID())
	}
	return assets
}

// UTXOs returns the UTXOs transaction is producing.
func (t *BaseTx) UTXOs() []*ava.UTXO {
	txID := t.ID()
	utxos := make([]*ava.UTXO, len(t.Outs))
	for i, out := range t.Outs {
		utxos[i] = &ava.UTXO{
			UTXOID: ava.UTXOID{
				TxID:        txID,
				OutputIndex: uint32(i),
			},
			Asset: ava.Asset{ID: out.AssetID()},
			Out:   out.Out,
		}
	}
	return utxos
}

// SyntacticVerify that this transaction is well-formed.
func (t *BaseTx) SyntacticVerify(ctx *snow.Context, c codec.Codec, _ int) error {
	switch {
	case t == nil:
		return errNilTx
	case t.NetID != ctx.NetworkID:
		return errWrongNetworkID
	case !t.BCID.Equals(ctx.ChainID):
		return errWrongChainID
	}

	fc := ava.NewFlowChecker()
	for _, out := range t.Outs {
		if err := out.Verify(); err != nil {
			return err
		}
		fc.Produce(out.AssetID(), out.Output().Amount())
	}
	if !IsSortedTransferableOutputs(t.Outs, c) {
		return errOutputsNotSorted
	}

	for _, in := range t.Ins {
		if err := in.Verify(); err != nil {
			return err
		}
		fc.Consume(in.AssetID(), in.Input().Amount())
	}
	if !isSortedAndUniqueTransferableInputs(t.Ins) {
		return errInputsNotSortedUnique
	}

	// TODO: Add the Tx fee to the produced side

	if err := fc.Verify(); err != nil {
		return err
	}

	return t.Metadata.Verify()
}

// SemanticVerify that this transaction is valid to be spent.
func (t *BaseTx) SemanticVerify(vm *VM, uTx *UniqueTx, creds []verify.Verifiable) error {
	for i, in := range t.Ins {
		cred := creds[i]

		fxIndex, err := vm.getFx(cred)
		if err != nil {
			return err
		}
		fx := vm.fxs[fxIndex].Fx

		utxo, err := vm.getUTXO(&in.UTXOID)
		if err != nil {
			return err
		}

		utxoAssetID := utxo.AssetID()
		inAssetID := in.AssetID()
		if !utxoAssetID.Equals(inAssetID) {
			return errAssetIDMismatch
		}

		if !vm.verifyFxUsage(fxIndex, inAssetID) {
			return errIncompatibleFx
		}

		if err := fx.VerifyTransfer(uTx, utxo.Out, in.In, cred); err != nil {
			return err
		}
	}
	return nil
}

// ExecuteWithSideEffects writes the batch with any additional side effects
func (t *BaseTx) ExecuteWithSideEffects(_ *VM, batch database.Batch) error { return batch.Write() }
