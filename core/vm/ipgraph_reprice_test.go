// Copyright 2025 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package vm

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// The reprice fork only meters the external (Ext) traversal selectors; the internal
// selectors keep their static RequiredGas price. These tests pin both sides.

// ipNode returns a deterministic, distinct address for graph node k.
func ipNode(k int) common.Address {
	return common.BigToAddress(big.NewInt(int64(0x100000 + k)))
}

func ptrU64(v uint64) *uint64 { return &v }

// storyRepriceConfig returns a Story chain config with the ipgraph reprice fork
// scheduled at repriceTime (nil = never scheduled, i.e. old flat pricing).
func storyRepriceConfig(repriceTime *uint64) *params.ChainConfig {
	cfg := *params.MergedTestChainConfig
	cfg.ChainID = new(big.Int).SetUint64(params.IDStoryLocal)
	cfg.IPGraphRepriceTime = repriceTime
	return &cfg
}

func whitelistACL(statedb StateDB, caller common.Address) {
	aclKey := new(big.Int)
	aclKey.SetString(aclSlot, 16)
	aclKey = crypto.Keccak256Hash(caller.Bytes(), aclKey.Bytes()).Big()
	statedb.SetState(aclAddress, common.BigToHash(aclKey), common.BigToHash(big.NewInt(1)))
}

// buildChainGraph writes a depth-D ancestor chain into ipgraph storage
// (node0 -> node1 -> ... -> node(D-1)) and whitelists the caller in the ACL.
// A chain of D nodes makes getRoyalty do exactly 3D-2 traversal reads:
// (2D-1) in topologicalSort + (D-1) royalty reads in the LAP loop.
func buildChainGraph(statedb StateDB, depth int, caller common.Address) (leaf, root common.Address) {
	for k := 0; k < depth-1; k++ {
		node := ipNode(k)
		statedb.SetState(ipGraphAddress, common.BytesToHash(node.Bytes()), common.BigToHash(big.NewInt(1)))
		slot := crypto.Keccak256Hash(node.Bytes()).Big()
		statedb.SetState(ipGraphAddress, common.BigToHash(slot), common.BytesToHash(ipNode(k+1).Bytes()))
	}
	whitelistACL(statedb, caller)
	return ipNode(0), ipNode(depth - 1)
}

func getRoyaltyLAPInput(selector []byte, leaf, root common.Address) []byte {
	in := append([]byte{}, selector...)
	in = append(in, common.LeftPadBytes(leaf.Bytes(), 32)...)
	in = append(in, common.LeftPadBytes(root.Bytes(), 32)...)
	in = append(in, make([]byte, 32)...) // royaltyPolicyKind = LAP (0)
	return in
}

func newRepriceEVM(statedb StateDB, cfg *params.ChainConfig, caller common.Address, blockTime uint64) *EVM {
	vmctx := BlockContext{
		CanTransfer: func(StateDB, common.Address, *uint256.Int) bool { return true },
		Transfer:    func(StateDB, common.Address, common.Address, *uint256.Int) {},
		BlockNumber: big.NewInt(1),
		Time:        blockTime,
		Random:      &common.Hash{}, // post-merge rules
	}
	evm := NewEVM(vmctx, statedb, cfg, Config{})
	evm.caller = caller
	return evm
}

// runGetRoyalty runs getRoyalty (or getRoyaltyExt, per selector) end-to-end through
// RunPrecompiledContract and returns the gas consumed.
func runGetRoyalty(t *testing.T, cfg *params.ChainConfig, selector []byte, depth int, supplied, blockTime uint64) (consumed uint64, err error) {
	t.Helper()
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	caller := common.HexToAddress("0xcafe")
	leaf, root := buildChainGraph(statedb, depth, caller)
	evm := newRepriceEVM(statedb, cfg, caller, blockTime)
	_, remaining, err := RunPrecompiledContract(evm, &ipGraph{}, getRoyaltyLAPInput(selector, leaf, root), supplied, nil)
	return supplied - remaining, err
}

const (
	getRoyaltyInternalLAPGas = uint64(ipGraphReadGas * averageAncestorIpCount * 3)         // 900
	getRoyaltyExtLAPGas      = uint64(ipGraphExternalReadGas * averageAncestorIpCount * 3) // 189000 (pre-fork flat Ext)
)

// TestIPGraphRepriceInternalStaysStatic pins that the internal getRoyalty selector is
// NOT repriced: it charges the same flat constant regardless of graph size or whether
// the fork is active. (Internal traversal selectors are reserved for ACL-gated callers
// and keep their static pricing; only the external Ext variants are metered.)
func TestIPGraphRepriceInternalStaysStatic(t *testing.T) {
	const supplied = 100_000_000
	for _, tc := range []struct {
		name string
		cfg  *params.ChainConfig
	}{
		{"fork-off", storyRepriceConfig(nil)},
		{"fork-on", storyRepriceConfig(ptrU64(0))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c50, err := runGetRoyalty(t, tc.cfg, getRoyaltySelector, 50, supplied, 1000)
			if err != nil {
				t.Fatalf("depth 50: %v", err)
			}
			c500, err := runGetRoyalty(t, tc.cfg, getRoyaltySelector, 500, supplied, 1000)
			if err != nil {
				t.Fatalf("depth 500: %v", err)
			}
			if c50 != getRoyaltyInternalLAPGas || c500 != getRoyaltyInternalLAPGas {
				t.Fatalf("internal getRoyalty must stay the flat %d regardless of depth/fork; got depth50=%d depth500=%d",
					getRoyaltyInternalLAPGas, c50, c500)
			}
		})
	}
}

// TestIPGraphRepriceExtMeters pins the post-fork behaviour of the external selector:
// every traversal read is charged at the cold-SLOAD price, so gas scales with graph
// size and equals (3D-1)*2100 (1 ACL floor read + (3D-2) metered traversal reads).
func TestIPGraphRepriceExtMeters(t *testing.T) {
	cfg := storyRepriceConfig(ptrU64(0)) // active from genesis
	const supplied = 1_000_000_000

	for _, depth := range []int{10, 100, 200} {
		consumed, err := runGetRoyalty(t, cfg, getRoyaltyExtSelector, depth, supplied, 1000)
		if err != nil {
			t.Fatalf("depth %d: %v", depth, err)
		}
		reads := uint64(3*depth - 1) // 1 ACL floor + (3D-2) metered traversal reads
		want := reads * ipGraphColdReadGas
		if consumed != want {
			t.Fatalf("depth %d: consumed %d, want %d*%d = %d", depth, consumed, reads, ipGraphColdReadGas, want)
		}
	}

	// Sanity: deeper graph must cost strictly more (unlike the pre-fork constant), and
	// a small graph must cost far less than the old flat Ext price.
	c10, _ := runGetRoyalty(t, cfg, getRoyaltyExtSelector, 10, supplied, 1000)
	c200, _ := runGetRoyalty(t, cfg, getRoyaltyExtSelector, 200, supplied, 1000)
	if c10 >= c200 {
		t.Fatalf("post-fork gas must scale with graph size: depth10=%d depth200=%d", c10, c200)
	}
	if c10 >= getRoyaltyExtLAPGas {
		t.Fatalf("small-graph Ext call should be cheaper than the old flat Ext price %d; got %d", getRoyaltyExtLAPGas, c10)
	}
}

// TestIPGraphRepriceExtOOG pins that when the budget can't cover the traversal, the
// metered read returns ErrOutOfGas and the whole call reverts — it must never return
// a partial royalty (which would break consensus and gas estimation).
func TestIPGraphRepriceExtOOG(t *testing.T) {
	cfg := storyRepriceConfig(ptrU64(0))
	// depth 100 needs 3*100-2 = 298 traversal reads; fund only ~50 reads past the base.
	supplied := ipGraphColdReadGas + 50*ipGraphColdReadGas
	_, err := runGetRoyalty(t, cfg, getRoyaltyExtSelector, 100, supplied, 1000)
	if !errors.Is(err, ErrOutOfGas) {
		t.Fatalf("want ErrOutOfGas when budget can't cover the traversal, got %v", err)
	}
}

// TestIPGraphRepriceNotYetActive checks the time gate: with a future reprice time, a
// block before it still charges the external selector its old flat price.
func TestIPGraphRepriceNotYetActive(t *testing.T) {
	cfg := storyRepriceConfig(ptrU64(5000)) // activates at t=5000
	const supplied = 100_000_000

	consumed, err := runGetRoyalty(t, cfg, getRoyaltyExtSelector, 300, supplied, 1000) // block time 1000 < 5000
	if err != nil {
		t.Fatalf("pre-activation: %v", err)
	}
	if consumed != getRoyaltyExtLAPGas {
		t.Fatalf("before activation the old flat Ext price must apply; got %d want %d", consumed, getRoyaltyExtLAPGas)
	}
}

// TestIPGraphRepriceStackExtMetered pins that the external getRoyaltyStackExt selector
// is metered consistently: one ACL-read floor plus per-read metering. For LRP over a
// node with P parents the cost is (2 + 2P) cold reads: 1 ACL + 1 length + P*(parent +
// royalty). The internal getRoyaltyStack keeps its static price.
func TestIPGraphRepriceStackExtMetered(t *testing.T) {
	cfg := storyRepriceConfig(ptrU64(0))
	const supplied = 1_000_000_000
	const parents = 5

	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	caller := common.HexToAddress("0xcafe")
	whitelistACL(statedb, caller)

	ipID := ipNode(0)
	statedb.SetState(ipGraphAddress, common.BytesToHash(ipID.Bytes()), common.BigToHash(big.NewInt(parents)))
	for i := 0; i < parents; i++ {
		slot := crypto.Keccak256Hash(ipID.Bytes()).Big()
		slot.Add(slot, big.NewInt(int64(i)))
		statedb.SetState(ipGraphAddress, common.BigToHash(slot), common.BytesToHash(ipNode(i+1).Bytes()))
	}

	evm := newRepriceEVM(statedb, cfg, caller, 1000)
	in := append([]byte{}, getRoyaltyStackExtSelector...)
	in = append(in, common.LeftPadBytes(ipID.Bytes(), 32)...)
	in = append(in, common.LeftPadBytes(royaltyPolicyKindLRP.Bytes(), 32)...)

	_, remaining, err := RunPrecompiledContract(evm, &ipGraph{}, in, supplied, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumed := uint64(supplied) - remaining
	reads := uint64(2 + 2*parents) // 1 ACL floor + 1 length + parents*(parent + royalty)
	want := reads * ipGraphColdReadGas
	if consumed != want {
		t.Fatalf("getRoyaltyStackExt LRP post-fork: consumed %d, want (2+2*%d)*%d = %d",
			consumed, parents, ipGraphColdReadGas, want)
	}
}

// TestIPGraphRepriceWriteUnaffected pins that the per-read reprice only touches the
// metered Ext traversal selectors: a write selector (addParentIp) keeps its static
// write price after the fork.
func TestIPGraphRepriceWriteUnaffected(t *testing.T) {
	cfg := storyRepriceConfig(ptrU64(0))
	const supplied = 1_000_000_000

	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	caller := common.HexToAddress("0xcafe")
	whitelistACL(statedb, caller)

	parents := []common.Address{ipNode(1), ipNode(2), ipNode(3)}
	in := append([]byte{}, addParentIpSelector...)
	in = append(in, common.LeftPadBytes(ipNode(0).Bytes(), 32)...)
	in = append(in, common.LeftPadBytes(big.NewInt(0x40).Bytes(), 32)...) // offset (ignored)
	in = append(in, common.LeftPadBytes(big.NewInt(int64(len(parents))).Bytes(), 32)...)
	for _, p := range parents {
		in = append(in, common.LeftPadBytes(p.Bytes(), 32)...)
	}

	evm := newRepriceEVM(statedb, cfg, caller, 1000)
	evm.currentPrecompileCallType = CALL

	_, remaining, err := RunPrecompiledContract(evm, &ipGraph{}, in, supplied, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumed := uint64(supplied) - remaining
	want := uint64(intrinsicGas + ipGraphWriteGas*len(parents))
	if consumed != want {
		t.Fatalf("addParentIp post-fork: consumed %d, want static write price %d", consumed, want)
	}
}
