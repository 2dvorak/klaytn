// Modifications Copyright 2018 The klaytn Authors
// Copyright 2015 The go-ethereum Authors
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
//
// This file is derived from core/tx_pool_test.go (2018/06/04).
// Modified and improved for the klaytn development.

package blockchain

import (
	"crypto/ecdsa"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/klaytn/klaytn/rlp"

	"github.com/klaytn/klaytn/fork"

	"github.com/klaytn/klaytn/blockchain/state"
	"github.com/klaytn/klaytn/blockchain/types"
	"github.com/klaytn/klaytn/common"
	"github.com/klaytn/klaytn/crypto"
	"github.com/klaytn/klaytn/event"
	"github.com/klaytn/klaytn/params"
	"github.com/klaytn/klaytn/storage/database"
	"github.com/stretchr/testify/assert"
)

var (
	// testTxPoolConfig is a transaction pool configuration without stateful disk
	// sideeffects used during testing.
	testTxPoolConfig TxPoolConfig

	// eip1559Config is a chain config with EIP-1559 enabled at block 0.
	eip1559Config *params.ChainConfig
)

func init() {
	testTxPoolConfig = DefaultTxPoolConfig
	testTxPoolConfig.Journal = ""

	cpy := *params.TestChainConfig
	eip1559Config = &cpy
	eip1559Config.IstanbulCompatibleBlock = common.Big0
	eip1559Config.LondonCompatibleBlock = common.Big0
	eip1559Config.EthTxTypeCompatibleBlock = common.Big0
	fork.SetHardForkBlockNumberConfig(eip1559Config)
}

type testBlockChain struct {
	statedb       *state.StateDB
	gasLimit      uint64
	chainHeadFeed *event.Feed
}

func (bc *testBlockChain) CurrentBlock() *types.Block {
	return types.NewBlock(&types.Header{}, nil, nil)
}

func (bc *testBlockChain) GetBlock(hash common.Hash, number uint64) *types.Block {
	return bc.CurrentBlock()
}

func (bc *testBlockChain) StateAt(common.Hash) (*state.StateDB, error) {
	return bc.statedb, nil
}

func (bc *testBlockChain) SubscribeChainHeadEvent(ch chan<- ChainHeadEvent) event.Subscription {
	return bc.chainHeadFeed.Subscribe(ch)
}

func transaction(nonce uint64, gaslimit uint64, key *ecdsa.PrivateKey) *types.Transaction {
	return pricedTransaction(nonce, gaslimit, big.NewInt(1), key)
}

func pricedTransaction(nonce uint64, gaslimit uint64, gasprice *big.Int, key *ecdsa.PrivateKey) *types.Transaction {
	tx, _ := types.SignTx(types.NewTransaction(nonce, common.HexToAddress("0xAAAA"), big.NewInt(100), gaslimit, gasprice, nil),
		types.LatestSignerForChainID(params.TestChainConfig.ChainID), key)
	return tx
}

func dynamicFeeTx(nonce uint64, gaslimit uint64, gasFee *big.Int, tip *big.Int, key *ecdsa.PrivateKey) *types.Transaction {
	dynamicTx := types.NewTx(&types.TxInternalDataEthereumDynamicFee{
		ChainID:      params.TestChainConfig.ChainID,
		AccountNonce: nonce,
		GasTipCap:    tip,
		GasFeeCap:    gasFee,
		GasLimit:     gaslimit,
		Recipient:    &common.Address{},
		Amount:       big.NewInt(100),
		Payload:      nil,
		AccessList:   nil,
	})

	signedTx, _ := types.SignTx(dynamicTx, types.LatestSignerForChainID(params.TestChainConfig.ChainID), key)
	return signedTx
}

func feeDelegatedTx(nonce uint64, gaslimit uint64, gasPrice *big.Int, amount *big.Int, senderPrvKey *ecdsa.PrivateKey, feePayerPrvKey *ecdsa.PrivateKey) *types.Transaction {
	delegatedTx := types.NewTx(&types.TxInternalDataFeeDelegatedValueTransfer{
		AccountNonce: nonce,
		Price:        gasPrice,
		GasLimit:     gaslimit,
		Recipient:    common.Address{},
		Amount:       amount,
		From:         crypto.PubkeyToAddress(senderPrvKey.PublicKey),
		FeePayer:     crypto.PubkeyToAddress(feePayerPrvKey.PublicKey),
	})

	types.SignTxAsFeePayer(delegatedTx, types.LatestSignerForChainID(params.TestChainConfig.ChainID), feePayerPrvKey)
	signedTx, _ := types.SignTx(delegatedTx, types.LatestSignerForChainID(params.TestChainConfig.ChainID), senderPrvKey)
	return signedTx
}

func setupTxPool() (*TxPool, *ecdsa.PrivateKey) {
	return setupTxPoolWithConfig(params.TestChainConfig)
}

func setupTxPoolWithConfig(config *params.ChainConfig) (*TxPool, *ecdsa.PrivateKey) {
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil)
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	key, _ := crypto.GenerateKey()
	pool := NewTxPool(testTxPoolConfig, config, blockchain)

	return pool, key
}

// validateTxPoolInternals checks various consistency invariants within the pool.
func validateTxPoolInternals(pool *TxPool) error {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	// Ensure the total transaction set is consistent with pending + queued
	pending, queued := pool.stats()
	if total := len(pool.all); total != pending+queued {
		return fmt.Errorf("total transaction count %d != %d pending + %d queued", total, pending, queued)
	}
	if priced := pool.priced.items.Len() - pool.priced.stales; priced != pending+queued {
		return fmt.Errorf("total priced transaction count %d != %d pending + %d queued", priced, pending, queued)
	}
	// Ensure the next nonce to assign is the correct one
	for addr, txs := range pool.pending {
		// Find the last transaction
		var last uint64
		for nonce := range txs.txs.items {
			if last < nonce {
				last = nonce
			}
		}
		if nonce := pool.getPendingNonce(addr); nonce != last+1 {
			return fmt.Errorf("pending nonce mismatch: have %v, want %v", nonce, last+1)
		}
	}
	return nil
}

// validateEvents checks that the correct number of transaction addition events
// were fired on the pool's event feed.
func validateEvents(events chan NewTxsEvent, count int) error {
	var received []*types.Transaction

	for len(received) < count {
		select {
		case ev := <-events:
			received = append(received, ev.Txs...)
		case <-time.After(time.Second):
			return fmt.Errorf("event #%d not fired", len(received))
		}
	}
	if len(received) > count {
		return fmt.Errorf("more than %d events fired: %v", count, received[count:])
	}
	select {
	case ev := <-events:
		return fmt.Errorf("more than %d events fired: %v", count, ev.Txs)

	case <-time.After(50 * time.Millisecond):
		// This branch should be "default", but it's a data race between goroutines,
		// reading the event channel and pushing into it, so better wait a bit ensuring
		// really nothing gets injected.
	}
	return nil
}

func deriveSender(tx *types.Transaction) (common.Address, error) {
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	return types.Sender(signer, tx)
}

type testChain struct {
	*testBlockChain
	address common.Address
	trigger *bool
}

// testChain.State() is used multiple times to reset the pending state.
// when simulate is true it will create a state that indicates
// that tx0 and tx1 are included in the chain.
func (c *testChain) State() (*state.StateDB, error) {
	// delay "state change" by one. The tx pool fetches the
	// state multiple times and by delaying it a bit we simulate
	// a state change between those fetches.
	stdb := c.statedb
	if *c.trigger {
		c.statedb, _ = state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil)
		// simulate that the new head block included tx0 and tx1
		c.statedb.SetNonce(c.address, 2)
		c.statedb.SetBalance(c.address, new(big.Int).SetUint64(params.KLAY))
		*c.trigger = false
	}
	return stdb, nil
}

// This test simulates a scenario where a new block is imported during a
// state reset and tests whether the pending state is in sync with the
// block head event that initiated the resetState().
func TestStateChangeDuringTransactionPoolReset(t *testing.T) {
	t.Parallel()

	var (
		key, _     = crypto.GenerateKey()
		address    = crypto.PubkeyToAddress(key.PublicKey)
		statedb, _ = state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil)
		trigger    = false
	)

	// setup pool with 2 transaction in it
	statedb.SetBalance(address, new(big.Int).SetUint64(params.KLAY))
	blockchain := &testChain{&testBlockChain{statedb, 1000000000, new(event.Feed)}, address, &trigger}

	tx0 := transaction(0, 100000, key)
	tx1 := transaction(1, 100000, key)

	pool := NewTxPool(testTxPoolConfig, params.TestChainConfig, blockchain)
	defer pool.Stop()

	nonce := pool.GetPendingNonce(address)
	if nonce != 0 {
		t.Fatalf("Invalid nonce, want 0, got %d", nonce)
	}

	pool.AddRemotes(types.Transactions{tx0, tx1})

	nonce = pool.GetPendingNonce(address)
	if nonce != 2 {
		t.Fatalf("Invalid nonce, want 2, got %d", nonce)
	}

	// trigger state change in the background
	trigger = true

	pool.lockedReset(nil, nil)

	_, err := pool.Pending()
	if err != nil {
		t.Fatalf("Could not fetch pending transactions: %v", err)
	}
	nonce = pool.GetPendingNonce(address)
	if nonce != 2 {
		t.Fatalf("Invalid nonce, want 2, got %d", nonce)
	}
}

func testAddBalance(pool *TxPool, addr common.Address, amount *big.Int) {
	pool.mu.Lock()
	pool.currentState.AddBalance(addr, amount)
	pool.mu.Unlock()
}

func testSetNonce(pool *TxPool, addr common.Address, nonce uint64) {
	pool.mu.Lock()
	pool.currentState.SetNonce(addr, nonce)
	pool.mu.Unlock()
}

func TestInvalidTransactions(t *testing.T) {
	t.Parallel()

	pool, key := setupTxPool()
	defer pool.Stop()

	tx := transaction(0, 100, key)
	from, _ := deriveSender(tx)

	testAddBalance(pool, from, big.NewInt(1))
	if err := pool.AddRemote(tx); err != ErrInsufficientFundsFrom {
		t.Error("expected", ErrInsufficientFundsFrom)
	}

	balance := new(big.Int).Add(tx.Value(), new(big.Int).Mul(new(big.Int).SetUint64(tx.Gas()), tx.GasPrice()))
	testAddBalance(pool, from, balance)
	if err := pool.AddRemote(tx); err != ErrIntrinsicGas {
		t.Error("expected", ErrIntrinsicGas, "got", err)
	}

	testSetNonce(pool, from, 1)
	testAddBalance(pool, from, big.NewInt(0xffffffffffffff))
	tx = transaction(0, 100000, key)
	if err := pool.AddRemote(tx); err != ErrNonceTooLow {
		t.Error("expected", ErrNonceTooLow)
	}

	tx = transaction(1, 100000, key)
	pool.gasPrice = big.NewInt(1000)

	// NOTE-Klaytn We only accept txs with an expected gas price only
	//         regardless of local or remote.
	if err := pool.AddRemote(tx); err != ErrInvalidUnitPrice {
		t.Error("expected", ErrInvalidUnitPrice, "got", err)
	}
	if err := pool.AddLocal(tx); err != ErrInvalidUnitPrice {
		t.Error("expected", ErrInvalidUnitPrice, "got", err)
	}
}

func genAnchorTx(nonce uint64) *types.Transaction {
	key, _ := crypto.HexToECDSA("45a915e4d060149eb4365960e6a7a45f334393093061116b197e3240065ff2d8")
	from := crypto.PubkeyToAddress(key.PublicKey)

	gasLimit := uint64(1000000)
	gasPrice := big.NewInt(1)

	data := []byte{0x11, 0x22}
	values := map[types.TxValueKeyType]interface{}{
		types.TxValueKeyNonce:        nonce,
		types.TxValueKeyFrom:         from,
		types.TxValueKeyGasLimit:     gasLimit,
		types.TxValueKeyGasPrice:     gasPrice,
		types.TxValueKeyAnchoredData: data,
	}

	tx, _ := types.NewTransactionWithMap(types.TxTypeChainDataAnchoring, values)

	signer := types.MakeSigner(params.BFTTestChainConfig, big.NewInt(2))
	tx.Sign(signer, key)

	return tx
}

func TestAnchorTransactions(t *testing.T) {
	t.Parallel()

	pool, _ := setupTxPool()
	defer pool.Stop()

	poolAllow, _ := setupTxPool()
	poolAllow.config.AllowLocalAnchorTx = true
	defer poolAllow.Stop()

	tx1 := genAnchorTx(1)
	tx2 := genAnchorTx(2)

	from, _ := tx1.From()
	testAddBalance(pool, from, big.NewInt(10000000))
	testAddBalance(poolAllow, from, big.NewInt(10000000))

	// default txPool
	{
		err := pool.AddRemote(tx1)
		assert.NoError(t, err)

		err = pool.AddLocal(tx2)
		assert.Error(t, errNotAllowedAnchoringTx, err)
	}

	// txPool which allow locally submitted anchor txs
	{
		err := poolAllow.AddRemote(tx1)
		assert.NoError(t, err)

		err = poolAllow.AddLocal(tx2)
		assert.NoError(t, err)
	}
}

func TestTransactionQueue(t *testing.T) {
	t.Parallel()

	pool, key := setupTxPool()
	defer pool.Stop()

	tx := transaction(0, 100, key)
	from, _ := deriveSender(tx)
	testAddBalance(pool, from, big.NewInt(1000))
	pool.lockedReset(nil, nil)
	pool.enqueueTx(tx.Hash(), tx)

	pool.promoteExecutables([]common.Address{from})
	if len(pool.pending) != 1 {
		t.Error("expected valid txs to be 1 is", len(pool.pending))
	}

	tx = transaction(1, 100, key)
	from, _ = deriveSender(tx)
	testSetNonce(pool, from, 2)
	pool.enqueueTx(tx.Hash(), tx)
	pool.promoteExecutables([]common.Address{from})
	if _, ok := pool.pending[from].txs.items[tx.Nonce()]; ok {
		t.Error("expected transaction to be in tx pool")
	}

	if len(pool.queue) > 0 {
		t.Error("expected transaction queue to be empty. is", len(pool.queue))
	}

	pool, key = setupTxPool()
	defer pool.Stop()

	tx1 := transaction(0, 100, key)
	tx2 := transaction(10, 100, key)
	tx3 := transaction(11, 100, key)
	from, _ = deriveSender(tx1)
	pool.currentState.AddBalance(from, big.NewInt(1000))
	pool.lockedReset(nil, nil)

	pool.enqueueTx(tx1.Hash(), tx1)
	pool.enqueueTx(tx2.Hash(), tx2)
	pool.enqueueTx(tx3.Hash(), tx3)

	pool.promoteExecutables([]common.Address{from})

	if len(pool.pending) != 1 {
		t.Error("expected tx pool to be 1, got", len(pool.pending))
	}
	if pool.queue[from].Len() != 2 {
		t.Error("expected len(queue) == 2, got", pool.queue[from].Len())
	}
}

func TestTransactionNegativeValue(t *testing.T) {
	t.Parallel()

	pool, key := setupTxPool()
	defer pool.Stop()

	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	tx, _ := types.SignTx(types.NewTransaction(0, common.Address{}, big.NewInt(-1), 100, big.NewInt(1), nil), signer, key)
	from, _ := deriveSender(tx)
	pool.currentState.AddBalance(from, big.NewInt(1))
	if err := pool.AddRemote(tx); err != ErrNegativeValue {
		t.Error("expected", ErrNegativeValue, "got", err)
	}
}

func TestTransactionChainFork(t *testing.T) {
	t.Parallel()

	pool, key := setupTxPool()
	defer pool.Stop()

	addr := crypto.PubkeyToAddress(key.PublicKey)
	resetState := func() {
		statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil)
		statedb.AddBalance(addr, big.NewInt(100000000000000))

		pool.chain = &testBlockChain{statedb, 1000000, new(event.Feed)}
		pool.lockedReset(nil, nil)
	}
	resetState()

	tx := transaction(0, 100000, key)
	if _, err := pool.add(tx, false); err != nil {
		t.Error("didn't expect error", err)
	}
	pool.removeTx(tx.Hash(), true)

	// reset the pool's internal state
	resetState()
	if _, err := pool.add(tx, false); err != nil {
		t.Error("didn't expect error", err)
	}
}

func TestTransactionDoubleNonce(t *testing.T) {
	t.Parallel()

	pool, key := setupTxPool()
	defer pool.Stop()

	addr := crypto.PubkeyToAddress(key.PublicKey)
	resetState := func() {
		statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil)
		statedb.AddBalance(addr, big.NewInt(100000000000000))

		pool.chain = &testBlockChain{statedb, 1000000, new(event.Feed)}
		pool.lockedReset(nil, nil)
	}
	resetState()

	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	tx1, _ := types.SignTx(types.NewTransaction(0, common.HexToAddress("0xAAAA"), big.NewInt(100), 100000, big.NewInt(1), nil), signer, key)
	tx2, _ := types.SignTx(types.NewTransaction(0, common.HexToAddress("0xAAAA"), big.NewInt(100), 1000000, big.NewInt(2), nil), signer, key)
	tx3, _ := types.SignTx(types.NewTransaction(0, common.HexToAddress("0xAAAA"), big.NewInt(100), 1000000, big.NewInt(1), nil), signer, key)

	// NOTE-Klaytn Add the first two transaction, ensure the first one stays only
	if replace, err := pool.add(tx1, false); err != nil || replace {
		t.Errorf("first transaction insert failed (%v) or reported replacement (%v)", err, replace)
	}
	if replace, err := pool.add(tx2, false); err == nil || replace {
		t.Errorf("second transaction insert failed (%v) or not reported replacement (%v)", err, replace)
	}
	pool.promoteExecutables([]common.Address{addr})
	if pool.pending[addr].Len() != 1 {
		t.Error("expected 1 pending transactions, got", pool.pending[addr].Len())
	}
	if tx := pool.pending[addr].txs.items[0]; tx.Hash() != tx1.Hash() {
		t.Errorf("transaction mismatch: have %x, want %x", tx.Hash(), tx2.Hash())
	}
	// NOTE-Klaytn Add the third transaction and ensure it's not saved
	pool.add(tx3, false)
	pool.promoteExecutables([]common.Address{addr})
	if pool.pending[addr].Len() != 1 {
		t.Error("expected 1 pending transactions, got", pool.pending[addr].Len())
	}
	if tx := pool.pending[addr].txs.items[0]; tx.Hash() != tx1.Hash() {
		t.Errorf("transaction mismatch: have %x, want %x", tx.Hash(), tx2.Hash())
	}
	// Ensure the total transaction count is correct
	if len(pool.all) != 1 {
		t.Error("expected 1 total transactions, got", len(pool.all))
	}
}

func TestTransactionMissingNonce(t *testing.T) {
	t.Parallel()

	pool, key := setupTxPool()
	defer pool.Stop()

	addr := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, addr, big.NewInt(100000000000000))
	tx := transaction(1, 100000, key)
	if _, err := pool.add(tx, false); err != nil {
		t.Error("didn't expect error", err)
	}
	if len(pool.pending) != 0 {
		t.Error("expected 0 pending transactions, got", len(pool.pending))
	}
	if pool.queue[addr].Len() != 1 {
		t.Error("expected 1 queued transaction, got", pool.queue[addr].Len())
	}
	if len(pool.all) != 1 {
		t.Error("expected 1 total transactions, got", len(pool.all))
	}
}

func TestTransactionNonceRecovery(t *testing.T) {
	t.Parallel()

	const n = 10
	pool, key := setupTxPool()
	defer pool.Stop()

	addr := crypto.PubkeyToAddress(key.PublicKey)
	testSetNonce(pool, addr, n)
	testAddBalance(pool, addr, big.NewInt(100000000000000))
	pool.lockedReset(nil, nil)

	tx := transaction(n, 100000, key)
	if err := pool.AddRemote(tx); err != nil {
		t.Error(err)
	}
	// simulate some weird re-order of transactions and missing nonce(s)
	testSetNonce(pool, addr, n-1)
	pool.lockedReset(nil, nil)
	if fn := pool.GetPendingNonce(addr); fn != n-1 {
		t.Errorf("expected nonce to be %d, got %d", n-1, fn)
	}
}

// Tests that if an account runs out of funds, any pending and queued transactions
// are dropped.
func TestTransactionDropping(t *testing.T) {
	t.Parallel()

	// Create a test account and fund it
	pool, key := setupTxPool()
	defer pool.Stop()

	account, _ := deriveSender(transaction(0, 0, key))
	testAddBalance(pool, account, big.NewInt(1000))

	// Add some pending and some queued transactions
	var (
		tx0  = transaction(0, 100, key)
		tx1  = transaction(1, 200, key)
		tx2  = transaction(2, 300, key)
		tx10 = transaction(10, 100, key)
		tx11 = transaction(11, 200, key)
		tx12 = transaction(12, 300, key)
	)
	pool.promoteTx(account, tx0.Hash(), tx0)
	pool.promoteTx(account, tx1.Hash(), tx1)
	pool.promoteTx(account, tx2.Hash(), tx2)
	pool.enqueueTx(tx10.Hash(), tx10)
	pool.enqueueTx(tx11.Hash(), tx11)
	pool.enqueueTx(tx12.Hash(), tx12)

	// Check that pre and post validations leave the pool as is
	if pool.pending[account].Len() != 3 {
		t.Errorf("pending transaction mismatch: have %d, want %d", pool.pending[account].Len(), 3)
	}
	if pool.queue[account].Len() != 3 {
		t.Errorf("queued transaction mismatch: have %d, want %d", pool.queue[account].Len(), 3)
	}
	if len(pool.all) != 6 {
		t.Errorf("total transaction mismatch: have %d, want %d", len(pool.all), 6)
	}
	pool.lockedReset(nil, nil)
	if pool.pending[account].Len() != 3 {
		t.Errorf("pending transaction mismatch: have %d, want %d", pool.pending[account].Len(), 3)
	}
	if pool.queue[account].Len() != 3 {
		t.Errorf("queued transaction mismatch: have %d, want %d", pool.queue[account].Len(), 3)
	}
	if len(pool.all) != 6 {
		t.Errorf("total transaction mismatch: have %d, want %d", len(pool.all), 6)
	}
	// Reduce the balance of the account, and check that invalidated transactions are dropped
	testAddBalance(pool, account, big.NewInt(-650))
	pool.lockedReset(nil, nil)

	if _, ok := pool.pending[account].txs.items[tx0.Nonce()]; !ok {
		t.Errorf("funded pending transaction missing: %v", tx0)
	}
	if _, ok := pool.pending[account].txs.items[tx1.Nonce()]; !ok {
		t.Errorf("funded pending transaction missing: %v", tx0)
	}
	if _, ok := pool.pending[account].txs.items[tx2.Nonce()]; ok {
		t.Errorf("out-of-fund pending transaction present: %v", tx1)
	}
	if _, ok := pool.queue[account].txs.items[tx10.Nonce()]; !ok {
		t.Errorf("funded queued transaction missing: %v", tx10)
	}
	if _, ok := pool.queue[account].txs.items[tx11.Nonce()]; !ok {
		t.Errorf("funded queued transaction missing: %v", tx10)
	}
	if _, ok := pool.queue[account].txs.items[tx12.Nonce()]; ok {
		t.Errorf("out-of-fund queued transaction present: %v", tx11)
	}
	if len(pool.all) != 4 {
		t.Errorf("total transaction mismatch: have %d, want %d", len(pool.all), 4)
	}
}

// Tests that if a transaction is dropped from the current pending pool (e.g. out
// of fund), all consecutive (still valid, but not executable) transactions are
// postponed back into the future queue to prevent broadcasting them.
func TestTransactionPostponing(t *testing.T) {
	t.Parallel()

	// Create the pool to test the postponing with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil)
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	pool := NewTxPool(testTxPoolConfig, params.TestChainConfig, blockchain)
	defer pool.Stop()

	// Create two test accounts to produce different gap profiles with
	keys := make([]*ecdsa.PrivateKey, 2)
	accs := make([]common.Address, len(keys))

	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		accs[i] = crypto.PubkeyToAddress(keys[i].PublicKey)

		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(50100))
	}
	// Add a batch consecutive pending transactions for validation
	txs := []*types.Transaction{}
	for i, key := range keys {

		for j := 0; j < 100; j++ {
			var tx *types.Transaction
			if (i+j)%2 == 0 {
				tx = transaction(uint64(j), 25000, key)
			} else {
				tx = transaction(uint64(j), 50000, key)
			}
			txs = append(txs, tx)
		}
	}
	for i, err := range pool.AddRemotes(txs) {
		if err != nil {
			t.Fatalf("tx %d: failed to add transactions: %v", i, err)
		}
	}
	// Check that pre and post validations leave the pool as is
	if pending := pool.pending[accs[0]].Len() + pool.pending[accs[1]].Len(); pending != len(txs) {
		t.Errorf("pending transaction mismatch: have %d, want %d", pending, len(txs))
	}
	if len(pool.queue) != 0 {
		t.Errorf("queued accounts mismatch: have %d, want %d", len(pool.queue), 0)
	}
	if len(pool.all) != len(txs) {
		t.Errorf("total transaction mismatch: have %d, want %d", len(pool.all), len(txs))
	}
	pool.lockedReset(nil, nil)
	if pending := pool.pending[accs[0]].Len() + pool.pending[accs[1]].Len(); pending != len(txs) {
		t.Errorf("pending transaction mismatch: have %d, want %d", pending, len(txs))
	}
	if len(pool.queue) != 0 {
		t.Errorf("queued accounts mismatch: have %d, want %d", len(pool.queue), 0)
	}
	if len(pool.all) != len(txs) {
		t.Errorf("total transaction mismatch: have %d, want %d", len(pool.all), len(txs))
	}
	// Reduce the balance of the account, and check that transactions are reorganised
	for _, addr := range accs {
		testAddBalance(pool, addr, big.NewInt(-1))
	}
	pool.lockedReset(nil, nil)

	// The first account's first transaction remains valid, check that subsequent
	// ones are either filtered out, or queued up for later.
	if _, ok := pool.pending[accs[0]].txs.items[txs[0].Nonce()]; !ok {
		t.Errorf("tx %d: valid and funded transaction missing from pending pool: %v", 0, txs[0])
	}
	if _, ok := pool.queue[accs[0]].txs.items[txs[0].Nonce()]; ok {
		t.Errorf("tx %d: valid and funded transaction present in future queue: %v", 0, txs[0])
	}
	for i, tx := range txs[1:100] {
		if i%2 == 1 {
			if _, ok := pool.pending[accs[0]].txs.items[tx.Nonce()]; ok {
				t.Errorf("tx %d: valid but future transaction present in pending pool: %v", i+1, tx)
			}
			if _, ok := pool.queue[accs[0]].txs.items[tx.Nonce()]; !ok {
				t.Errorf("tx %d: valid but future transaction missing from future queue: %v", i+1, tx)
			}
		} else {
			if _, ok := pool.pending[accs[0]].txs.items[tx.Nonce()]; ok {
				t.Errorf("tx %d: out-of-fund transaction present in pending pool: %v", i+1, tx)
			}
			if _, ok := pool.queue[accs[0]].txs.items[tx.Nonce()]; ok {
				t.Errorf("tx %d: out-of-fund transaction present in future queue: %v", i+1, tx)
			}
		}
	}
	// The second account's first transaction got invalid, check that all transactions
	// are either filtered out, or queued up for later.
	if pool.pending[accs[1]] != nil {
		t.Errorf("invalidated account still has pending transactions")
	}
	for i, tx := range txs[100:] {
		if i%2 == 1 {
			if _, ok := pool.queue[accs[1]].txs.items[tx.Nonce()]; !ok {
				t.Errorf("tx %d: valid but future transaction missing from future queue: %v", 100+i, tx)
			}
		} else {
			if _, ok := pool.queue[accs[1]].txs.items[tx.Nonce()]; ok {
				t.Errorf("tx %d: out-of-fund transaction present in future queue: %v", 100+i, tx)
			}
		}
	}
	if len(pool.all) != len(txs)/2 {
		t.Errorf("total transaction mismatch: have %d, want %d", len(pool.all), len(txs)/2)
	}
}

// Tests that if the transaction pool has both executable and non-executable
// transactions from an origin account, filling the nonce gap moves all queued
// ones into the pending pool.
func TestTransactionGapFilling(t *testing.T) {
	t.Parallel()

	// Create a test account and fund it
	pool, key := setupTxPool()
	defer pool.Stop()

	account, _ := deriveSender(transaction(0, 0, key))
	testAddBalance(pool, account, big.NewInt(1000000))

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan NewTxsEvent, testTxPoolConfig.NonExecSlotsAccount+5)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a pending and a queued transaction with a nonce-gap in between
	if err := pool.AddRemote(transaction(0, 100000, key)); err != nil {
		t.Fatalf("failed to add pending transaction: %v", err)
	}
	if err := pool.AddRemote(transaction(2, 100000, key)); err != nil {
		t.Fatalf("failed to add queued transaction: %v", err)
	}
	pending, queued := pool.Stats()
	if pending != 1 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 1)
	}
	if queued != 1 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
	}
	if err := validateEvents(events, 1); err != nil {
		t.Fatalf("original event firing failed: %v", err)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Fill the nonce gap and ensure all transactions become pending
	if err := pool.AddRemote(transaction(1, 100000, key)); err != nil {
		t.Fatalf("failed to add gapped transaction: %v", err)
	}
	pending, queued = pool.Stats()
	if pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("gap-filling event firing failed: %v", err)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that if the transaction count belonging to a single account goes above
// some threshold, the higher transactions are dropped to prevent DOS attacks.
func TestTransactionQueueAccountLimiting(t *testing.T) {
	t.Parallel()

	// Create a test account and fund it
	pool, key := setupTxPool()
	defer pool.Stop()

	account, _ := deriveSender(transaction(0, 0, key))
	testAddBalance(pool, account, big.NewInt(1000000))

	// Keep queuing up transactions and make sure all above a limit are dropped
	for i := uint64(1); i <= testTxPoolConfig.NonExecSlotsAccount+5; i++ {
		if err := pool.AddRemote(transaction(i, 100000, key)); err != nil {
			t.Fatalf("tx %d: failed to add transaction: %v", i, err)
		}
		if len(pool.pending) != 0 {
			t.Errorf("tx %d: pending pool size mismatch: have %d, want %d", i, len(pool.pending), 0)
		}
		if i <= testTxPoolConfig.NonExecSlotsAccount {
			if pool.queue[account].Len() != int(i) {
				t.Errorf("tx %d: queue size mismatch: have %d, want %d", i, pool.queue[account].Len(), i)
			}
		} else {
			if pool.queue[account].Len() != int(testTxPoolConfig.NonExecSlotsAccount) {
				t.Errorf("tx %d: queue limit mismatch: have %d, want %d", i, pool.queue[account].Len(), testTxPoolConfig.NonExecSlotsAccount)
			}
		}
	}
	if len(pool.all) != int(testTxPoolConfig.NonExecSlotsAccount) {
		t.Errorf("total transaction mismatch: have %d, want %d", len(pool.all), testTxPoolConfig.NonExecSlotsAccount)
	}
}

// Tests that if the transaction count belonging to multiple accounts go above
// some threshold, the higher transactions are dropped to prevent DOS attacks.
//
// This logic should not hold for local transactions, unless the local tracking
// mechanism is disabled.
func TestTransactionQueueGlobalLimiting(t *testing.T) {
	testTransactionQueueGlobalLimiting(t, false)
}
func TestTransactionQueueGlobalLimitingNoLocals(t *testing.T) {
	testTransactionQueueGlobalLimiting(t, true)
}

func testTransactionQueueGlobalLimiting(t *testing.T, nolocals bool) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil)
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	config := testTxPoolConfig
	config.NoLocals = nolocals
	config.NonExecSlotsAll = config.NonExecSlotsAccount*3 - 1 // reduce the queue limits to shorten test time (-1 to make it non divisible)

	pool := NewTxPool(config, params.TestChainConfig, blockchain)
	defer pool.Stop()

	// Create a number of test accounts and fund them (last one will be the local)
	keys := make([]*ecdsa.PrivateKey, 5)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	local := keys[len(keys)-1]

	// Generate and queue a batch of transactions
	nonces := make(map[common.Address]uint64)

	txs := make(types.Transactions, 0, 3*config.NonExecSlotsAll)
	for len(txs) < cap(txs) {
		key := keys[rand.Intn(len(keys)-1)] // skip adding transactions with the local account
		addr := crypto.PubkeyToAddress(key.PublicKey)

		txs = append(txs, transaction(nonces[addr]+1, 100000, key))
		nonces[addr]++
	}
	// Import the batch and verify that limits have been enforced
	pool.AddRemotes(txs)

	queued := 0
	for addr, list := range pool.queue {
		if list.Len() > int(config.NonExecSlotsAccount) {
			t.Errorf("addr %x: queued accounts overflown allowance: %d > %d", addr, list.Len(), config.NonExecSlotsAccount)
		}
		queued += list.Len()
	}
	if queued > int(config.NonExecSlotsAll) {
		t.Fatalf("total transactions overflow allowance: %d > %d", queued, config.NonExecSlotsAll)
	}
	// Generate a batch of transactions from the local account and import them
	txs = txs[:0]
	for i := uint64(0); i < 3*config.NonExecSlotsAll; i++ {
		txs = append(txs, transaction(i+1, 100000, local))
	}
	pool.AddLocals(txs)

	// If locals are disabled, the previous eviction algorithm should apply here too
	if nolocals {
		queued := 0
		for addr, list := range pool.queue {
			if list.Len() > int(config.NonExecSlotsAccount) {
				t.Errorf("addr %x: queued accounts overflown allowance: %d > %d", addr, list.Len(), config.NonExecSlotsAccount)
			}
			queued += list.Len()
		}
		if queued > int(config.NonExecSlotsAll) {
			t.Fatalf("total transactions overflow allowance: %d > %d", queued, config.NonExecSlotsAll)
		}
	} else {
		// Local exemptions are enabled, make sure the local account owned the queue
		if len(pool.queue) != 1 {
			t.Errorf("multiple accounts in queue: have %v, want %v", len(pool.queue), 1)
		}
		// Also ensure no local transactions are ever dropped, even if above global limits
		if queued := pool.queue[crypto.PubkeyToAddress(local.PublicKey)].Len(); uint64(queued) != 3*config.NonExecSlotsAll {
			t.Fatalf("local account queued transaction count mismatch: have %v, want %v", queued, 3*config.NonExecSlotsAll)
		}
	}
}

// Tests that if an account remains idle for a prolonged amount of time, any
// non-executable transactions queued up are dropped to prevent wasting resources
// on shuffling them around.
//
// This logic should not hold for local transactions, unless the local tracking
// mechanism is disabled.
func TestTransactionQueueTimeLimitingKeepLocals(t *testing.T) {
	testTransactionQueueTimeLimiting(t, false, true)
}
func TestTransactionQueueTimeLimitingNotKeepLocals(t *testing.T) {
	testTransactionQueueTimeLimiting(t, false, false)
}
func TestTransactionQueueTimeLimitingNoLocalsKeepLocals(t *testing.T) {
	testTransactionQueueTimeLimiting(t, true, true)
}
func TestTransactionQueueTimeLimitingNoLocalsNoKeepLocals(t *testing.T) {
	testTransactionQueueTimeLimiting(t, true, false)
}

func testTransactionQueueTimeLimiting(t *testing.T, nolocals, keepLocals bool) {
	// Reduce the eviction interval to a testable amount
	defer func(old time.Duration) { evictionInterval = old }(evictionInterval)
	evictionInterval = time.Second

	// Create the pool to test the non-expiration enforcement
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil)
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	config := testTxPoolConfig
	config.Lifetime = 5 * time.Second
	config.NoLocals = nolocals
	config.KeepLocals = keepLocals

	pool := NewTxPool(config, params.TestChainConfig, blockchain)
	defer pool.Stop()

	// Create two test accounts to ensure remotes expire but locals do not
	local, _ := crypto.GenerateKey()
	remote, _ := crypto.GenerateKey()

	testAddBalance(pool, crypto.PubkeyToAddress(local.PublicKey), big.NewInt(1000000000))
	testAddBalance(pool, crypto.PubkeyToAddress(remote.PublicKey), big.NewInt(1000000000))

	// Add the two transactions and ensure they both are queued up
	if err := pool.AddLocal(pricedTransaction(1, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add local transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(1, 100000, big.NewInt(1), remote)); err != nil {
		t.Fatalf("failed to add remote transaction: %v", err)
	}
	pending, queued := pool.Stats()
	if pending != 0 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
	}
	if queued != 2 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 2)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}

	time.Sleep(2 * evictionInterval)

	// Wait a bit for eviction, but queued transactions must remain.
	pending, queued = pool.Stats()
	assert.Equal(t, pending, 0)
	assert.Equal(t, queued, 2)

	// Wait a bit for eviction to run and clean up any leftovers, and ensure only the local remains
	time.Sleep(2 * config.Lifetime)

	pending, queued = pool.Stats()
	assert.Equal(t, pending, 0)

	if nolocals {
		assert.Equal(t, queued, 0)
	} else {
		if keepLocals {
			assert.Equal(t, queued, 1)
		} else {
			assert.Equal(t, queued, 0)
		}
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that even if the transaction count belonging to a single account goes
// above some threshold, as long as the transactions are executable, they are
// accepted.
func TestTransactionPendingLimiting(t *testing.T) {
	t.Parallel()

	// Create a test account and fund it
	pool, key := setupTxPool()
	defer pool.Stop()

	account, _ := deriveSender(transaction(0, 0, key))
	testAddBalance(pool, account, big.NewInt(1000000))

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan NewTxsEvent, testTxPoolConfig.NonExecSlotsAccount+5)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Keep queuing up transactions and make sure all above a limit are dropped
	for i := uint64(0); i < testTxPoolConfig.NonExecSlotsAccount+5; i++ {
		if err := pool.AddRemote(transaction(i, 100000, key)); err != nil {
			t.Fatalf("tx %d: failed to add transaction: %v", i, err)
		}
		if pool.pending[account].Len() != int(i)+1 {
			t.Errorf("tx %d: pending pool size mismatch: have %d, want %d", i, pool.pending[account].Len(), i+1)
		}
		if len(pool.queue) != 0 {
			t.Errorf("tx %d: queue size mismatch: have %d, want %d", i, pool.queue[account].Len(), 0)
		}
	}
	if len(pool.all) != int(testTxPoolConfig.NonExecSlotsAccount+5) {
		t.Errorf("total transaction mismatch: have %d, want %d", len(pool.all), testTxPoolConfig.NonExecSlotsAccount+5)
	}
	if err := validateEvents(events, int(testTxPoolConfig.NonExecSlotsAccount+5)); err != nil {
		t.Fatalf("event firing failed: %v", err)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that the transaction limits are enforced the same way irrelevant whether
// the transactions are added one by one or in batches.
func TestTransactionQueueLimitingEquivalency(t *testing.T) { testTransactionLimitingEquivalency(t, 1) }
func TestTransactionPendingLimitingEquivalency(t *testing.T) {
	testTransactionLimitingEquivalency(t, 0)
}

func testTransactionLimitingEquivalency(t *testing.T, origin uint64) {
	t.Parallel()

	// Add a batch of transactions to a pool one by one
	pool1, key1 := setupTxPool()
	defer pool1.Stop()

	account1, _ := deriveSender(transaction(0, 0, key1))
	testAddBalance(pool1, account1, big.NewInt(1000000))

	for i := uint64(0); i < testTxPoolConfig.NonExecSlotsAccount+5; i++ {
		if err := pool1.AddRemote(transaction(origin+i, 100000, key1)); err != nil {
			t.Fatalf("tx %d: failed to add transaction: %v", i, err)
		}
	}
	// Add a batch of transactions to a pool in one big batch
	pool2, key2 := setupTxPool()
	defer pool2.Stop()

	account2, _ := deriveSender(transaction(0, 0, key2))
	testAddBalance(pool2, account2, big.NewInt(1000000))

	txs := []*types.Transaction{}
	for i := uint64(0); i < testTxPoolConfig.NonExecSlotsAccount+5; i++ {
		txs = append(txs, transaction(origin+i, 100000, key2))
	}
	pool2.AddRemotes(txs)

	// Ensure the batch optimization honors the same pool mechanics
	if len(pool1.pending) != len(pool2.pending) {
		t.Errorf("pending transaction count mismatch: one-by-one algo: %d, batch algo: %d", len(pool1.pending), len(pool2.pending))
	}
	if len(pool1.queue) != len(pool2.queue) {
		t.Errorf("queued transaction count mismatch: one-by-one algo: %d, batch algo: %d", len(pool1.queue), len(pool2.queue))
	}
	if len(pool1.all) != len(pool2.all) {
		t.Errorf("total transaction count mismatch: one-by-one algo %d, batch algo %d", len(pool1.all), len(pool2.all))
	}
	if err := validateTxPoolInternals(pool1); err != nil {
		t.Errorf("pool 1 internal state corrupted: %v", err)
	}
	if err := validateTxPoolInternals(pool2); err != nil {
		t.Errorf("pool 2 internal state corrupted: %v", err)
	}
}

// Tests that if the transaction count belonging to multiple accounts go above
// some hard threshold, the higher transactions are dropped to prevent DOS
// attacks.
func TestTransactionPendingGlobalLimiting(t *testing.T) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil)
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	config := testTxPoolConfig
	config.ExecSlotsAll = config.ExecSlotsAccount * 10

	pool := NewTxPool(config, params.TestChainConfig, blockchain)
	defer pool.Stop()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 5)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions
	nonces := make(map[common.Address]uint64)

	txs := types.Transactions{}
	for _, key := range keys {
		addr := crypto.PubkeyToAddress(key.PublicKey)
		for j := 0; j < int(config.ExecSlotsAll)/len(keys)*2; j++ {
			txs = append(txs, transaction(nonces[addr], 100000, key))
			nonces[addr]++
		}
	}
	// Import the batch and verify that limits have been enforced
	pool.AddRemotes(txs)

	pending := 0
	for _, list := range pool.pending {
		pending += list.Len()
	}
	if pending > int(config.ExecSlotsAll) {
		t.Fatalf("total pending transactions overflow allowance: %d > %d", pending, config.ExecSlotsAll)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that if transactions start being capped, transactions are also removed from 'all'
func TestTransactionCapClearsFromAll(t *testing.T) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil)
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	config := testTxPoolConfig
	config.ExecSlotsAccount = 2
	config.NonExecSlotsAccount = 2
	config.ExecSlotsAll = 8

	pool := NewTxPool(config, params.TestChainConfig, blockchain)
	defer pool.Stop()

	// Create a number of test accounts and fund them
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	pool.currentState.AddBalance(addr, big.NewInt(1000000))

	txs := types.Transactions{}
	for j := 0; j < int(config.ExecSlotsAll)*2; j++ {
		txs = append(txs, transaction(uint64(j), 100000, key))
	}
	// Import the batch and verify that limits have been enforced
	pool.AddRemotes(txs)
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that if the transaction count belonging to multiple accounts go above
// some hard threshold, if they are under the minimum guaranteed slot count then
// the transactions are still kept.
func TestTransactionPendingMinimumAllowance(t *testing.T) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil)
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	config := testTxPoolConfig
	config.ExecSlotsAll = 0

	pool := NewTxPool(config, params.TestChainConfig, blockchain)
	defer pool.Stop()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 5)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions
	nonces := make(map[common.Address]uint64)

	txs := types.Transactions{}
	for _, key := range keys {
		addr := crypto.PubkeyToAddress(key.PublicKey)
		for j := 0; j < int(config.ExecSlotsAccount)*2; j++ {
			txs = append(txs, transaction(nonces[addr], 100000, key))
			nonces[addr]++
		}
	}
	// Import the batch and verify that limits have been enforced
	pool.AddRemotes(txs)

	for addr, list := range pool.pending {
		if list.Len() != int(config.ExecSlotsAccount) {
			t.Errorf("addr %x: total pending transactions mismatch: have %d, want %d", addr, list.Len(), config.ExecSlotsAccount)
		}
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// NOTE-Klaytn Disable test, because we don't have a pool repricing feature anymore.
// Tests that setting the transaction pool gas price to a higher value correctly
// discards everything cheaper than that and moves any gapped transactions back
// from the pending pool to the queue.
//
// Note, local transactions are never allowed to be dropped.
/*
func TestTransactionPoolRepricing(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemDB()))
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	pool := NewTxPool(testTxPoolConfig, params.TestChainConfig, blockchain)
	defer pool.Stop()

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 4)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions, both pending and queued
	txs := types.Transactions{}

	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(2), keys[0]))
	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(1), keys[0]))
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(2), keys[0]))

	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(1), keys[1]))
	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(2), keys[1]))
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(2), keys[1]))

	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(2), keys[2]))
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(1), keys[2]))
	txs = append(txs, pricedTransaction(3, 100000, big.NewInt(2), keys[2]))

	ltx := pricedTransaction(0, 100000, big.NewInt(1), keys[3])

	// Import the batch and that both pending and queued transactions match up
	pool.AddRemotes(txs)
	pool.AddLocal(ltx)

	pending, queued := pool.Stats()
	if pending != 7 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 7)
	}
	if queued != 3 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 3)
	}
	if err := validateEvents(events, 7); err != nil {
		t.Fatalf("original event firing failed: %v", err)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Reprice the pool and check that underpriced transactions get dropped
	pool.SetGasPrice(big.NewInt(2))

	pending, queued = pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 5 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 5)
	}
	if err := validateEvents(events, 0); err != nil {
		t.Fatalf("reprice event firing failed: %v", err)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// NOTE-Klaytn Klaytn currently accepts remote txs regardless of gas price.
	// TODO-Klaytn-RemoveLater Remove or uncomment the code below once the policy for how to
	//         deal with underpriced remote txs is decided.
	// Check that we can't add the old transactions back
	//if err := pool.AddRemote(pricedTransaction(1, 100000, big.NewInt(1), keys[0])); err != ErrUnderpriced {
	//	t.Fatalf("adding underpriced pending transaction error mismatch: have %v, want %v", err, ErrUnderpriced)
	//}
	//if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(1), keys[1])); err != ErrUnderpriced {
	//	t.Fatalf("adding underpriced pending transaction error mismatch: have %v, want %v", err, ErrUnderpriced)
	//}
	//if err := pool.AddRemote(pricedTransaction(2, 100000, big.NewInt(1), keys[2])); err != ErrUnderpriced {
	//	t.Fatalf("adding underpriced queued transaction error mismatch: have %v, want %v", err, ErrUnderpriced)
	//}
	if err := validateEvents(events, 0); err != nil {
		t.Fatalf("post-reprice event firing failed: %v", err)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// However we can add local underpriced transactions
	tx := pricedTransaction(1, 100000, big.NewInt(1), keys[3])
	if err := pool.AddLocal(tx); err != nil {
		t.Fatalf("failed to add underpriced local transaction: %v", err)
	}
	if pending, _ = pool.Stats(); pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if err := validateEvents(events, 1); err != nil {
		t.Fatalf("post-reprice local event firing failed: %v", err)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// And we can fill gaps with properly priced transactions
	if err := pool.AddRemote(pricedTransaction(1, 100000, big.NewInt(2), keys[0])); err != nil {
		t.Fatalf("failed to add pending transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(2), keys[1])); err != nil {
		t.Fatalf("failed to add pending transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(2, 100000, big.NewInt(2), keys[2])); err != nil {
		t.Fatalf("failed to add queued transaction: %v", err)
	}
	if err := validateEvents(events, 5); err != nil {
		t.Fatalf("post-reprice event firing failed: %v", err)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}
*/

// NOTE-GS Disable test, because we don't have a repricing policy
// TODO-Klaytn What's our rule for local transaction ?
// Tests that setting the transaction pool gas price to a higher value does not
// remove local transactions.
/*
func TestTransactionPoolRepricingKeepsLocals(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemDB()))
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	pool := NewTxPool(testTxPoolConfig, params.TestChainConfig, blockchain)
	defer pool.Stop()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 3)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000*1000000))
	}
	// Create transaction (both pending and queued) with a linearly growing gasprice
	for i := uint64(0); i < 500; i++ {
		// Add pending
		p_tx := pricedTransaction(i, 100000, big.NewInt(int64(i)), keys[2])
		if err := pool.AddLocal(p_tx); err != nil {
			t.Fatal(err)
		}
		// Add queued
		q_tx := pricedTransaction(i+501, 100000, big.NewInt(int64(i)), keys[2])
		if err := pool.AddLocal(q_tx); err != nil {
			t.Fatal(err)
		}
	}
	pending, queued := pool.Stats()
	expPending, expQueued := 500, 500
	validate := func() {
		pending, queued = pool.Stats()
		if pending != expPending {
			t.Fatalf("pending transactions mismatched: have %d, want %d", pending, expPending)
		}
		if queued != expQueued {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, expQueued)
		}

		if err := validateTxPoolInternals(pool); err != nil {
			t.Fatalf("pool internal state corrupted: %v", err)
		}
	}
	validate()

	// Reprice the pool and check that nothing is dropped
	pool.SetGasPrice(big.NewInt(2))
	validate()

	pool.SetGasPrice(big.NewInt(2))
	pool.SetGasPrice(big.NewInt(4))
	pool.SetGasPrice(big.NewInt(8))
	pool.SetGasPrice(big.NewInt(100))
	validate()
}
*/

// NOTE-Klaytn Disable test, because we accept only transactions with a expected
//         gas price and there is no underpricing policy anymore.
// Tests that when the pool reaches its global transaction limit, underpriced
// transactions are gradually shifted out for more expensive ones and any gapped
// pending transactions are moved into the queue.
//
// Note, local transactions are never allowed to be dropped.
/*
func TestTransactionPoolUnderpricing(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemDB()))
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	config := testTxPoolConfig
	config.ExecSlotsAll = 2
	config.NonExecSlotsAll = 2

	pool := NewTxPool(config, params.TestChainConfig, blockchain)
	defer pool.Stop()

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 4)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions, both pending and queued
	txs := types.Transactions{}

	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(1), keys[0]))
	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(2), keys[0]))

	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(1), keys[1]))

	ltx := pricedTransaction(0, 100000, big.NewInt(1), keys[2])

	// Import the batch and that both pending and queued transactions match up
	pool.AddRemotes(txs)
	pool.AddLocal(ltx)

	pending, queued := pool.Stats()
	if pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if queued != 1 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
	}
	if err := validateEvents(events, 3); err != nil {
		t.Fatalf("original event firing failed: %v", err)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Ensure that adding an underpriced transaction on block limit fails
	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(1), keys[1])); err != ErrUnderpriced {
		t.Fatalf("adding underpriced pending transaction error mismatch: have %v, want %v", err, ErrUnderpriced)
	}
	// Ensure that adding high priced transactions drops cheap ones, but not own
	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(3), keys[1])); err != nil { // +K1:0 => -K1:1 => Pend K0:0, K0:1, K1:0, K2:0; Que -
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(2, 100000, big.NewInt(4), keys[1])); err != nil { // +K1:2 => -K0:0 => Pend K1:0, K2:0; Que K0:1 K1:2
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(3, 100000, big.NewInt(5), keys[1])); err != nil { // +K1:3 => -K0:1 => Pend K1:0, K2:0; Que K1:2 K1:3
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	pending, queued = pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 2 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 2)
	}
	if err := validateEvents(events, 1); err != nil {
		t.Fatalf("additional event firing failed: %v", err)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Ensure that adding local transactions can push out even higher priced ones
	ltx = pricedTransaction(1, 100000, big.NewInt(0), keys[2])
	if err := pool.AddLocal(ltx); err != nil {
		t.Fatalf("failed to append underpriced local transaction: %v", err)
	}
	ltx = pricedTransaction(0, 100000, big.NewInt(0), keys[3])
	if err := pool.AddLocal(ltx); err != nil {
		t.Fatalf("failed to add new underpriced local transaction: %v", err)
	}
	pending, queued = pool.Stats()
	if pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if queued != 1 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("local event firing failed: %v", err)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}
*/

// NOTE-Klaytn Disable test, because we accept only transactions with a expected
//         gas price and there is no underpricing policy anymore.
// Tests that more expensive transactions push out cheap ones from the pool, but
// without producing instability by creating gaps that start jumping transactions
// back and forth between queued/pending.
/*
func TestTransactionPoolStableUnderpricing(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemDB()))
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	config := testTxPoolConfig
	config.ExecSlotsAll = 128
	config.NonExecSlotsAll = 0

	pool := NewTxPool(config, params.TestChainConfig, blockchain)
	defer pool.Stop()

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 2)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Fill up the entire queue with the same transaction price points
	txs := types.Transactions{}
	for i := uint64(0); i < config.ExecSlotsAll; i++ {
		txs = append(txs, pricedTransaction(i, 100000, big.NewInt(1), keys[0]))
	}
	pool.AddRemotes(txs)

	pending, queued := pool.Stats()
	if pending != int(config.ExecSlotsAll) {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, config.ExecSlotsAll)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validateEvents(events, int(config.ExecSlotsAll)); err != nil {
		t.Fatalf("original event firing failed: %v", err)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Ensure that adding high priced transactions drops a cheap, but doesn't produce a gap
	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(3), keys[1])); err != nil {
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	pending, queued = pool.Stats()
	if pending != int(config.ExecSlotsAll) {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, config.ExecSlotsAll)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validateEvents(events, 1); err != nil {
		t.Fatalf("additional event firing failed: %v", err)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}
*/

// NOTE-Klaytn Disable this test, because we don't have a replacement rule.
// Tests that the pool rejects replacement transactions that don't meet the minimum
// price bump required.
/*
func TestTransactionReplacement(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemDB()))
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	pool := NewTxPool(testTxPoolConfig, params.TestChainConfig, blockchain)
	defer pool.Stop()

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a test account to add transactions with
	key, _ := crypto.GenerateKey()
	pool.currentState.AddBalance(crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1000000000))

	// Add pending transactions, ensuring the minimum price bump is enforced for replacement (for ultra low prices too)
	price := int64(100)
	threshold := (price * (100 + int64(testTxPoolConfig.PriceBump))) / 100

	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(1), key)); err != nil {
		t.Fatalf("failed to add original cheap pending transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(0, 100001, big.NewInt(1), key)); err != ErrReplaceUnderpriced {
		t.Fatalf("original cheap pending transaction replacement error mismatch: have %v, want %v", err, ErrReplaceUnderpriced)
	}
	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(2), key)); err != nil {
		t.Fatalf("failed to replace original cheap pending transaction: %v", err)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("cheap replacement event firing failed: %v", err)
	}

	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(price), key)); err != nil {
		t.Fatalf("failed to add original proper pending transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(0, 100001, big.NewInt(threshold-1), key)); err != ErrReplaceUnderpriced {
		t.Fatalf("original proper pending transaction replacement error mismatch: have %v, want %v", err, ErrReplaceUnderpriced)
	}
	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(threshold), key)); err != nil {
		t.Fatalf("failed to replace original proper pending transaction: %v", err)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("proper replacement event firing failed: %v", err)
	}
	// Add queued transactions, ensuring the minimum price bump is enforced for replacement (for ultra low prices too)
	if err := pool.AddRemote(pricedTransaction(2, 100000, big.NewInt(1), key)); err != nil {
		t.Fatalf("failed to add original cheap queued transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(2, 100001, big.NewInt(1), key)); err != ErrReplaceUnderpriced {
		t.Fatalf("original cheap queued transaction replacement error mismatch: have %v, want %v", err, ErrReplaceUnderpriced)
	}
	if err := pool.AddRemote(pricedTransaction(2, 100000, big.NewInt(2), key)); err != nil {
		t.Fatalf("failed to replace original cheap queued transaction: %v", err)
	}

	if err := pool.AddRemote(pricedTransaction(2, 100000, big.NewInt(price), key)); err != nil {
		t.Fatalf("failed to add original proper queued transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(2, 100001, big.NewInt(threshold-1), key)); err != ErrReplaceUnderpriced {
		t.Fatalf("original proper queued transaction replacement error mismatch: have %v, want %v", err, ErrReplaceUnderpriced)
	}
	if err := pool.AddRemote(pricedTransaction(2, 100000, big.NewInt(threshold), key)); err != nil {
		t.Fatalf("failed to replace original proper queued transaction: %v", err)
	}

	if err := validateEvents(events, 0); err != nil {
		t.Fatalf("queued replacement event firing failed: %v", err)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}
*/

// Tests that local transactions are journaled to disk, but remote transactions
// get discarded between restarts.
func TestTransactionJournaling(t *testing.T)         { testTransactionJournaling(t, false) }
func TestTransactionJournalingNoLocals(t *testing.T) { testTransactionJournaling(t, true) }

func testTransactionJournaling(t *testing.T, nolocals bool) {
	t.Parallel()

	// Create a temporary file for the journal
	file, err := ioutil.TempFile("", "")
	if err != nil {
		t.Fatalf("failed to create temporary journal: %v", err)
	}
	journal := file.Name()
	defer os.Remove(journal)

	// Clean up the temporary file, we only need the path for now
	file.Close()
	os.Remove(journal)

	// Create the original pool to inject transaction into the journal
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil)
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	config := testTxPoolConfig
	config.NoLocals = nolocals
	config.Journal = journal
	config.JournalInterval = time.Second

	pool := NewTxPool(config, params.TestChainConfig, blockchain)

	// Create two test accounts to ensure remotes expire but locals do not
	local, _ := crypto.GenerateKey()
	remote, _ := crypto.GenerateKey()

	testAddBalance(pool, crypto.PubkeyToAddress(local.PublicKey), big.NewInt(1000000000))
	testAddBalance(pool, crypto.PubkeyToAddress(remote.PublicKey), big.NewInt(1000000000))

	// Add three local and a remote transactions and ensure they are queued up
	if err := pool.AddLocal(pricedTransaction(0, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add local transaction: %v", err)
	}
	if err := pool.AddLocal(pricedTransaction(1, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add local transaction: %v", err)
	}
	if err := pool.AddLocal(pricedTransaction(2, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add local transaction: %v", err)
	}
	if err := pool.AddRemote(pricedTransaction(0, 100000, big.NewInt(1), remote)); err != nil {
		t.Fatalf("failed to add remote transaction: %v", err)
	}
	pending, queued := pool.Stats()
	if pending != 4 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 4)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Terminate the old pool, bump the local nonce, create a new pool and ensure relevant transaction survive
	pool.Stop()
	statedb.SetNonce(crypto.PubkeyToAddress(local.PublicKey), 1)
	blockchain = &testBlockChain{statedb, 1000000, new(event.Feed)}

	pool = NewTxPool(config, params.TestChainConfig, blockchain)

	pending, queued = pool.Stats()
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if nolocals {
		if pending != 0 {
			t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
		}
	} else {
		if pending != 2 {
			t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
		}
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Bump the nonce temporarily and ensure the newly invalidated transaction is removed
	statedb.SetNonce(crypto.PubkeyToAddress(local.PublicKey), 2)
	pool.lockedReset(nil, nil)
	time.Sleep(2 * config.JournalInterval)
	pool.Stop()

	statedb.SetNonce(crypto.PubkeyToAddress(local.PublicKey), 1)
	blockchain = &testBlockChain{statedb, 1000000, new(event.Feed)}
	pool = NewTxPool(config, params.TestChainConfig, blockchain)

	pending, queued = pool.Stats()
	if pending != 0 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
	}
	if nolocals {
		if queued != 0 {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
		}
	} else {
		if queued != 1 {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
		}
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	pool.Stop()
}

// TestTransactionStatusCheck tests that the pool can correctly retrieve the
// pending status of individual transactions.
func TestTransactionStatusCheck(t *testing.T) {
	t.Parallel()

	// Create the pool to test the status retrievals with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil)
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	pool := NewTxPool(testTxPoolConfig, params.TestChainConfig, blockchain)
	defer pool.Stop()

	// Create the test accounts to check various transaction statuses with
	keys := make([]*ecdsa.PrivateKey, 3)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions, both pending and queued
	txs := types.Transactions{}

	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(1), keys[0])) // Pending only
	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(1), keys[1])) // Pending and queued
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(1), keys[1]))
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(1), keys[2])) // Queued only

	// Import the transaction and ensure they are correctly added
	pool.AddRemotes(txs)

	pending, queued := pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 2 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 2)
	}
	if err := validateTxPoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Retrieve the status of each transaction and validate them
	hashes := make([]common.Hash, len(txs))
	for i, tx := range txs {
		hashes[i] = tx.Hash()
	}
	hashes = append(hashes, common.Hash{})

	statuses := pool.Status(hashes)
	expect := []TxStatus{TxStatusPending, TxStatusPending, TxStatusQueued, TxStatusQueued, TxStatusUnknown}

	for i := 0; i < len(statuses); i++ {
		if statuses[i] != expect[i] {
			t.Errorf("transaction %d: status mismatch: have %v, want %v", i, statuses[i], expect[i])
		}
	}
}

func TestDynamicFeeTransactionVeryHighValues(t *testing.T) {
	t.Parallel()

	pool, key := setupTxPoolWithConfig(eip1559Config)
	defer pool.Stop()

	veryBigNumber := big.NewInt(1)
	veryBigNumber.Lsh(veryBigNumber, 300)

	tx := dynamicFeeTx(0, 100, big.NewInt(1), veryBigNumber, key)
	if err := pool.AddRemote(tx); err != ErrTipVeryHigh {
		t.Error("expected", ErrTipVeryHigh, "got", err)
	}

	tx2 := dynamicFeeTx(0, 100, veryBigNumber, big.NewInt(1), key)
	if err := pool.AddRemote(tx2); err != ErrFeeCapVeryHigh {
		t.Error("expected", ErrFeeCapVeryHigh, "got", err)
	}
}

func TestDynamicFeeTransactionHasNotSameGasPrice(t *testing.T) {
	t.Parallel()

	pool, key := setupTxPoolWithConfig(eip1559Config)
	defer pool.Stop()

	// Ensure gasFeeCap is greater than or equal to gasTipCap.
	tx := dynamicFeeTx(0, 100, big.NewInt(1), big.NewInt(2), key)
	if err := pool.AddRemote(tx); err != ErrTipAboveFeeCap {
		t.Error("expected", ErrTipAboveFeeCap, "got", err)
	}

	// The GasTipCap is equal to gasPrice that config at TxPool.
	tx2 := dynamicFeeTx(0, 100, big.NewInt(2), big.NewInt(2), key)
	if err := pool.AddRemote(tx2); err != ErrInvalidGasTipCap {
		t.Error("expected", ErrInvalidGasTipCap, "got", err)
	}
}

func TestDynamicFeeTransactionAccepted(t *testing.T) {
	t.Parallel()

	pool, key := setupTxPoolWithConfig(eip1559Config)
	defer pool.Stop()

	testAddBalance(pool, crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1000000))

	tx := dynamicFeeTx(0, 21000, big.NewInt(1), big.NewInt(1), key)
	if err := pool.AddRemote(tx); err != nil {
		t.Error("error", "got", err)
	}

	tx2 := dynamicFeeTx(1, 21000, big.NewInt(1), big.NewInt(1), key)
	if err := pool.AddRemote(tx2); err != nil {
		t.Error("error", "got", err)
	}
}

func TestDynamicFeeTransactionNotAccepted(t *testing.T) {
	t.Parallel()

	pool, key := setupTxPool()

	tx := dynamicFeeTx(0, 21000, big.NewInt(1), big.NewInt(1), key)
	if err := pool.AddRemote(tx); err != ErrTxTypeNotSupported {
		t.Error("expected", ErrTxTypeNotSupported, "got", err)
	}

	if err := pool.AddLocal(tx); err != ErrTxTypeNotSupported {
		t.Error("expected", ErrTxTypeNotSupported, "got", err)
	}
}

// TestFeeDelegatedTransaction checks feeDelegatedValueTransfer logic on tx pool
// the case when sender = feePayer has been included
func TestFeeDelegatedTransaction(t *testing.T) {
	t.Parallel()

	pool, key := setupTxPool()
	senderKey, _ := crypto.GenerateKey()
	feePayerKey, _ := crypto.GenerateKey()
	defer pool.Stop()

	testAddBalance(pool, crypto.PubkeyToAddress(senderKey.PublicKey), big.NewInt(60000))
	testAddBalance(pool, crypto.PubkeyToAddress(feePayerKey.PublicKey), big.NewInt(60000))

	tx := feeDelegatedTx(0, 40000, big.NewInt(1), big.NewInt(40000), senderKey, feePayerKey)

	if err := pool.AddRemote(tx); err != nil {
		t.Error("expected", "got", err)
	}

	testAddBalance(pool, crypto.PubkeyToAddress(key.PublicKey), big.NewInt(60000))

	// test on case when sender = feePayer
	// balance : 60k, tx.value : 40k, tx.fee : 40k
	tx1 := feeDelegatedTx(0, 40000, big.NewInt(1), big.NewInt(40000), key, key)

	// balance : 60k, tx.value : 10k, tx.fee : 40k
	tx2 := feeDelegatedTx(1, 40000, big.NewInt(1), big.NewInt(10000), key, key)

	if err := pool.AddRemote(tx1); err != ErrInsufficientFundsFrom {
		t.Error("expected", ErrInsufficientFundsFrom, "got", err)
	}

	if err := pool.AddRemote(tx2); err != nil {
		t.Error("expected", "got", err)
	}
}

func TestTransactionJournalingSortedByTime(t *testing.T) {
	t.Parallel()

	// Create a temporary file for the journal
	file, err := ioutil.TempFile("", "")
	if err != nil {
		t.Fatalf("failed to create temporary journal: %v", err)
	}
	journal := file.Name()
	defer os.Remove(journal)

	// Clean up the temporary file, we only need the path for now
	file.Close()
	os.Remove(journal)

	// Create the pool to test the status retrievals with
	statedb, _ := state.New(common.Hash{}, state.NewDatabase(database.NewMemoryDBManager()), nil)
	blockchain := &testBlockChain{statedb, 1000000, new(event.Feed)}

	config := testTxPoolConfig
	config.Journal = journal

	pool := NewTxPool(config, params.TestChainConfig, blockchain)
	defer pool.Stop()

	// Create the test accounts to check various transaction statuses with
	keys := make([]*ecdsa.PrivateKey, 3)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions, both pending and queued
	txs := types.Transactions{}

	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(1), keys[0])) // Pending only
	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(1), keys[1])) // Pending and queued
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(1), keys[1]))
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(1), keys[2])) // Queued only

	// Import the transaction locally and ensure they are correctly added
	pool.AddLocals(txs)

	// Execute rotate() to write transactions to file.
	pool.mu.Lock()
	if err := pool.journal.rotate(pool.local(), pool.signer); err != nil {
		t.Error("Failed to rotate local tx journal", "err", err)
	}
	pool.mu.Unlock()

	// Read a journal and load it.
	input, err := os.Open(pool.journal.path)
	if err != nil {
		t.Error(err)
	}
	defer input.Close()

	txsFromFile := make(types.Transactions, 4)
	stream := rlp.NewStream(input, 0)
	for i := 0; i < 4; i++ {
		tx := new(types.Transaction)
		if err = stream.Decode(tx); err != nil {
			if err != io.EOF {
				t.Error(err)
			}

			break
		}
		txsFromFile[i] = tx
	}

	// Check whether transactions loaded from journal file is sorted by time
	for i, tx := range txsFromFile {
		assert.Equal(t, txs[i].Hash(), tx.Hash())
		assert.False(t, txs[i].Time().Equal(tx.Time()))
	}
}

// Benchmarks the speed of validating the contents of the pending queue of the
// transaction pool.
func BenchmarkPendingDemotion100(b *testing.B)   { benchmarkPendingDemotion(b, 100) }
func BenchmarkPendingDemotion1000(b *testing.B)  { benchmarkPendingDemotion(b, 1000) }
func BenchmarkPendingDemotion10000(b *testing.B) { benchmarkPendingDemotion(b, 10000) }

func benchmarkPendingDemotion(b *testing.B, size int) {
	// Add a batch of transactions to a pool one by one
	pool, key := setupTxPool()
	defer pool.Stop()

	account, _ := deriveSender(transaction(0, 0, key))
	testAddBalance(pool, account, big.NewInt(1000000))

	for i := 0; i < size; i++ {
		tx := transaction(uint64(i), 100000, key)
		pool.promoteTx(account, tx.Hash(), tx)
	}
	// Benchmark the speed of pool validation
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.demoteUnexecutables()
	}
}

// Benchmarks the speed of scheduling the contents of the future queue of the
// transaction pool.
func BenchmarkFuturePromotion100(b *testing.B)   { benchmarkFuturePromotion(b, 100) }
func BenchmarkFuturePromotion1000(b *testing.B)  { benchmarkFuturePromotion(b, 1000) }
func BenchmarkFuturePromotion10000(b *testing.B) { benchmarkFuturePromotion(b, 10000) }

func benchmarkFuturePromotion(b *testing.B, size int) {
	// Add a batch of transactions to a pool one by one
	pool, key := setupTxPool()
	defer pool.Stop()

	account, _ := deriveSender(transaction(0, 0, key))
	testAddBalance(pool, account, big.NewInt(1000000))

	for i := 0; i < size; i++ {
		tx := transaction(uint64(1+i), 100000, key)
		pool.enqueueTx(tx.Hash(), tx)
	}
	// Benchmark the speed of pool validation
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.promoteExecutables(nil)
	}
}

// Benchmarks the speed of iterative transaction insertion.
func BenchmarkPoolInsert(b *testing.B) {
	// Generate a batch of transactions to enqueue into the pool
	pool, key := setupTxPool()
	defer pool.Stop()

	account, _ := deriveSender(transaction(0, 0, key))
	testAddBalance(pool, account, big.NewInt(1000000))

	txs := make(types.Transactions, b.N)
	for i := 0; i < b.N; i++ {
		txs[i] = transaction(uint64(i), 100000, key)
	}
	// Benchmark importing the transactions into the queue
	b.ResetTimer()
	for _, tx := range txs {
		pool.AddRemote(tx)
	}
}

// Benchmarks the speed of batched transaction insertion.
func BenchmarkPoolBatchInsert100(b *testing.B)   { benchmarkPoolBatchInsert(b, 100) }
func BenchmarkPoolBatchInsert1000(b *testing.B)  { benchmarkPoolBatchInsert(b, 1000) }
func BenchmarkPoolBatchInsert10000(b *testing.B) { benchmarkPoolBatchInsert(b, 10000) }

func benchmarkPoolBatchInsert(b *testing.B, size int) {
	// Generate a batch of transactions to enqueue into the pool
	pool, key := setupTxPool()
	defer pool.Stop()

	account, _ := deriveSender(transaction(0, 0, key))
	testAddBalance(pool, account, big.NewInt(1000000))

	batches := make([]types.Transactions, b.N)
	for i := 0; i < b.N; i++ {
		batches[i] = make(types.Transactions, size)
		for j := 0; j < size; j++ {
			batches[i][j] = transaction(uint64(size*i+j), 100000, key)
		}
	}
	// Benchmark importing the transactions into the queue
	b.ResetTimer()
	for _, batch := range batches {
		pool.AddRemotes(batch)
	}
}
