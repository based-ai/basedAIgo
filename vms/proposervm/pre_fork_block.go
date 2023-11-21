// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package proposervm

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/snow/choices"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/vms/proposervm/block"
)

var _ Block = (*preForkBlock)(nil)

type preForkBlock struct {
	snowman.Block
	vm *VM
}

func (b *preForkBlock) Accept(ctx context.Context) error {
	if err := b.acceptOuterBlk(); err != nil {
		return err
	}
	return b.acceptInnerBlk(ctx)
}

func (*preForkBlock) acceptOuterBlk() error {
	return nil
}

func (b *preForkBlock) acceptInnerBlk(ctx context.Context) error {
	return b.Block.Accept(ctx)
}

func (b *preForkBlock) Status() choices.Status {
	forkHeight, err := b.vm.getForkHeight()
	if err == database.ErrNotFound {
		return b.Block.Status()
	}
	if err != nil {
		// TODO: Once `Status()` can return an error, we should return the error
		// here.
		b.vm.ctx.Log.Error("unexpected error looking up fork height",
			zap.Error(err),
		)
		return b.Block.Status()
	}

	// The fork has occurred earlier than this block, so preForkBlocks are all
	// invalid.
	if b.Height() >= forkHeight {
		return choices.Rejected
	}
	return b.Block.Status()
}

func (b *preForkBlock) VerifyProposer(ctx context.Context) error {
	parent, err := b.vm.getPreForkBlock(ctx, b.Block.Parent())
	if err != nil {
		return err
	}
	return parent.verifyPreForkChild(ctx, b)
}

func (b *preForkBlock) Verify(ctx context.Context) error {
	return nil // Block verification is fully handled by VerifyProposer
}

func (b *preForkBlock) Options(ctx context.Context) ([2]snowman.Block, error) {
	oracleBlk, ok := b.Block.(snowman.OracleBlock)
	if !ok {
		return [2]snowman.Block{}, snowman.ErrNotOracle
	}

	options, err := oracleBlk.Options(ctx)
	if err != nil {
		return [2]snowman.Block{}, err
	}
	// A pre-fork block's child options are always pre-fork blocks
	return [2]snowman.Block{
		&preForkBlock{
			Block: options[0],
			vm:    b.vm,
		},
		&preForkBlock{
			Block: options[1],
			vm:    b.vm,
		},
	}, nil
}

func (b *preForkBlock) getInnerBlk() snowman.Block {
	return b.Block
}

func (b *preForkBlock) verifyPreForkChild(ctx context.Context, child *preForkBlock) error {
	parentTimestamp := b.Timestamp()
	if parentTimestamp.Before(b.vm.activationTime) {
		return nil
	}

	if err := verifyIsOracleBlock(ctx, b.Block); err != nil {
		return err
	}

	b.vm.ctx.Log.Debug("allowing pre-fork block after the fork time",
		zap.String("reason", "parent is an oracle block"),
		zap.Stringer("blkID", b.ID()),
	)
	return child.Block.Verify(ctx)
}

func (b *preForkBlock) verifyProposerPostForkChild(ctx context.Context, child *postForkBlock) error {
	if err := verifyIsNotOracleBlock(ctx, b.Block); err != nil {
		return err
	}

	childID := child.ID()
	childPChainHeight := child.PChainHeight()
	currentPChainHeight, err := b.vm.ctx.ValidatorState.GetCurrentHeight(ctx)
	if err != nil {
		b.vm.ctx.Log.Error("block verification failed",
			zap.String("reason", "failed to get current P-Chain height"),
			zap.Stringer("blkID", childID),
			zap.Error(err),
		)
		return err
	}
	if childPChainHeight > currentPChainHeight {
		return fmt.Errorf("%w: %d > %d",
			errPChainHeightNotReached,
			childPChainHeight,
			currentPChainHeight,
		)
	}
	if childPChainHeight < b.vm.minimumPChainHeight {
		return errPChainHeightTooLow
	}

	// Make sure [b] is the parent of [child]'s inner block
	expectedInnerParentID := b.ID()
	innerParentID := child.innerBlk.Parent()
	if innerParentID != expectedInnerParentID {
		return errInnerParentMismatch
	}

	// A *preForkBlock can only have a *postForkBlock child
	// if the *preForkBlock is the last *preForkBlock before activation takes effect
	// (its timestamp is at or after the activation time)
	parentTimestamp := b.Timestamp()
	if parentTimestamp.Before(b.vm.activationTime) {
		return errProposersNotActivated
	}

	// Child's timestamp must be at or after its parent's timestamp
	childTimestamp := child.Timestamp()
	if childTimestamp.Before(parentTimestamp) {
		return errTimeNotMonotonic
	}

	// Child timestamp can't be too far in the future
	maxTimestamp := b.vm.Time().Add(maxSkew)
	if childTimestamp.After(maxTimestamp) {
		return errTimeTooAdvanced
	}

	// Verify the lack of signature on the node
	return child.SignedBlock.Verify(false, b.vm.ctx.ChainID)
}

// This method only returns nil once (during the transition)
func (b *preForkBlock) verifyPostForkChild(ctx context.Context, child *postForkBlock) error {
	// Verify the inner block and track it as verified
	return b.vm.verifyAndRecordInnerBlk(ctx, nil, child)
}

func (*preForkBlock) verifyProposerPostForkOption(context.Context, *postForkOption) error {
	return errUnexpectedBlockType
}

func (*preForkBlock) verifyPostForkOption(context.Context, *postForkOption) error {
	return errUnexpectedBlockType
}

func (b *preForkBlock) buildChild(ctx context.Context) (Block, error) {
	parentTimestamp := b.Timestamp()
	if parentTimestamp.Before(b.vm.activationTime) {
		// The chain hasn't forked yet
		innerBlock, err := b.vm.ChainVM.BuildBlock(ctx)
		if err != nil {
			return nil, err
		}

		b.vm.ctx.Log.Info("built block",
			zap.Stringer("blkID", innerBlock.ID()),
			zap.Uint64("height", innerBlock.Height()),
			zap.Time("parentTimestamp", parentTimestamp),
		)

		return &preForkBlock{
			Block: innerBlock,
			vm:    b.vm,
		}, nil
	}

	// The chain is currently forking

	parentID := b.ID()
	newTimestamp := b.vm.Time().Truncate(time.Second)
	if newTimestamp.Before(parentTimestamp) {
		newTimestamp = parentTimestamp
	}

	// The child's P-Chain height is proposed as the optimal P-Chain height that
	// is at least the minimum height
	pChainHeight, err := b.vm.optimalPChainHeight(ctx, b.vm.minimumPChainHeight)
	if err != nil {
		b.vm.ctx.Log.Error("unexpected build block failure",
			zap.String("reason", "failed to calculate optimal P-chain height"),
			zap.Stringer("parentID", parentID),
			zap.Error(err),
		)
		return nil, err
	}

	innerBlock, err := b.vm.ChainVM.BuildBlock(ctx)
	if err != nil {
		return nil, err
	}

	statelessBlock, err := block.BuildUnsigned(
		parentID,
		newTimestamp,
		pChainHeight,
		innerBlock.Bytes(),
	)
	if err != nil {
		return nil, err
	}

	blk := &postForkBlock{
		SignedBlock: statelessBlock,
		postForkCommonComponents: postForkCommonComponents{
			vm:       b.vm,
			innerBlk: innerBlock,
			status:   choices.Processing,
		},
	}

	b.vm.ctx.Log.Info("built block",
		zap.Stringer("blkID", blk.ID()),
		zap.Stringer("innerBlkID", innerBlock.ID()),
		zap.Uint64("height", blk.Height()),
		zap.Time("parentTimestamp", parentTimestamp),
		zap.Time("blockTimestamp", newTimestamp))
	return blk, nil
}

func (*preForkBlock) pChainHeight(context.Context) (uint64, error) {
	return 0, nil
}
