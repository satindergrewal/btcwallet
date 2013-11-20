/*
 * Copyright (c) 2013 Conformal Systems LLC <info@conformal.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/conformal/btcjson"
	"github.com/conformal/btcwallet/wallet"
	"github.com/conformal/btcwire"
	"github.com/conformal/btcws"
	"time"
)

var (
	// ErrBtcdDisconnected describes an error where an operation cannot
	// successfully complete due to btcd not being connected to
	// btcwallet.
	ErrBtcdDisconnected = errors.New("btcd disconnected")
)

type cmdHandler func(chan []byte, btcjson.Cmd)

var rpcHandlers = map[string]cmdHandler{
	// Standard bitcoind methods
	"dumpprivkey":           DumpPrivKey,
	"dumpwallet":            DumpWallet,
	"getaddressesbyaccount": GetAddressesByAccount,
	"getbalance":            GetBalance,
	"getnewaddress":         GetNewAddress,
	"importprivkey":         ImportPrivKey,
	"listaccounts":          ListAccounts,
	"sendfrom":              SendFrom,
	"sendmany":              SendMany,
	"settxfee":              SetTxFee,
	"walletlock":            WalletLock,
	"walletpassphrase":      WalletPassphrase,

	// Extensions not exclusive to websocket connections.
	"createencryptedwallet": CreateEncryptedWallet,
}

// Extensions exclusive to websocket connections.
var wsHandlers = map[string]cmdHandler{
	"getbalances":    GetBalances,
	"walletislocked": WalletIsLocked,
}

// ProcessRequest checks the requests sent from a frontend.  If the
// request method is one that must be handled by btcwallet, the
// request is processed here.  Otherwise, the request is sent to btcd
// and btcd's reply is routed back to the frontend.
func ProcessRequest(frontend chan []byte, msg []byte, ws bool) {
	// Parse marshaled command and check
	cmd, err := btcjson.ParseMarshaledCmd(msg)
	if err != nil {
		// Check that msg is valid JSON-RPC.  Reply to frontend
		// with error if invalid.
		if cmd == nil {
			ReplyError(frontend, nil, &btcjson.ErrInvalidRequest)
			return
		}

		// btcwallet cannot handle this command, so defer handling
		// to btcd.
		DeferToBTCD(frontend, msg)
		return
	}

	// Check for a handler to reply to cmd.  If none exist, defer to btcd.
	if f, ok := rpcHandlers[cmd.Method()]; ok {
		f(frontend, cmd)
	} else if f, ok := wsHandlers[cmd.Method()]; ws && ok {
		f(frontend, cmd)
	} else {
		// btcwallet does not have a handler for the command.  Pass
		// to btcd and route replies back to the appropiate frontend.
		DeferToBTCD(frontend, msg)
	}
}

// DeferToBTCD sends an unmarshaled command to btcd, modifying the id
// and setting up a reply route to route the reply from btcd back to
// the frontend reply channel with the original id.
func DeferToBTCD(frontend chan []byte, msg []byte) {
	// msg cannot be sent to btcd directly, but the ID must instead be
	// changed to include additonal routing information so replies can
	// be routed back to the correct frontend.  Unmarshal msg into a
	// generic btcjson.Message struct so the ID can be modified and the
	// whole thing re-marshaled.
	var m btcjson.Message
	json.Unmarshal(msg, &m)

	// Create a new ID so replies can be routed correctly.
	n := <-NewJSONID
	var id interface{} = RouteID(m.Id, n)
	m.Id = &id

	// Marshal the request with modified ID.
	newMsg, err := json.Marshal(m)
	if err != nil {
		log.Errorf("DeferToBTCD: Cannot marshal message: %v", err)
		return
	}

	// If marshaling suceeded, save the id and frontend reply channel
	// so the reply can be sent to the correct frontend.
	replyRouter.Lock()
	replyRouter.m[n] = frontend
	replyRouter.Unlock()

	// Send message with modified ID to btcd.
	btcdMsgs <- newMsg
}

// RouteID creates a JSON-RPC id for a frontend request that was deferred
// to btcd.
func RouteID(origID, routeID interface{}) string {
	return fmt.Sprintf("btcwallet(%v)-%v", routeID, origID)
}

// ReplyError creates and marshals a btcjson.Reply with the error e,
// sending the reply to a frontend reply channel.
func ReplyError(frontend chan []byte, id interface{}, e *btcjson.Error) {
	// Create a Reply with a non-nil error to marshal.
	r := btcjson.Reply{
		Error: e,
		Id:    &id,
	}

	// Marshal reply and send to frontend if marshaling suceeded.
	if mr, err := json.Marshal(r); err == nil {
		frontend <- mr
	}
}

// ReplySuccess creates and marshals a btcjson.Reply with the result r,
// sending the reply to a frontend reply channel.
func ReplySuccess(frontend chan []byte, id interface{}, result interface{}) {
	// Create a Reply with a non-nil result to marshal.
	r := btcjson.Reply{
		Result: result,
		Id:     &id,
	}

	// Marshal reply and send to frontend if marshaling suceeded.
	if mr, err := json.Marshal(r); err == nil {
		frontend <- mr
	}
}

// DumpPrivKey replies to a dumpprivkey request with the private
// key for a single address, or an appropiate error if the wallet
// is locked.
func DumpPrivKey(frontend chan []byte, icmd btcjson.Cmd) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.DumpPrivKeyCmd)
	if !ok {
		ReplyError(frontend, icmd.Id(), &btcjson.ErrInternal)
		return
	}

	// Iterate over all accounts, returning the key if it is found
	// in any wallet.
	for _, a := range accounts.m {
		switch key, err := a.DumpWIFPrivateKey(cmd.Address); err {
		case wallet.ErrAddressNotFound:
			// Move on to the next account.
			continue

		case wallet.ErrWalletLocked:
			// Address was found, but the private key isn't
			// accessible.
			ReplyError(frontend, cmd.Id(), &btcjson.ErrWalletUnlockNeeded)
			return

		case nil:
			// Key was found.
			ReplySuccess(frontend, cmd.Id(), key)
			return

		default: // all other non-nil errors
			e := &btcjson.Error{
				Code:    btcjson.ErrWallet.Code,
				Message: err.Error(),
			}
			ReplyError(frontend, cmd.Id(), e)
			return
		}
	}

	// If this is reached, all accounts have been checked, but none
	// have they address.
	e := &btcjson.Error{
		Code:    btcjson.ErrWallet.Code,
		Message: "Address does not refer to a key",
	}
	ReplyError(frontend, cmd.Id(), e)
}

// DumpWallet replies to a dumpwallet request with all private keys
// in a wallet, or an appropiate error if the wallet is locked.
func DumpWallet(frontend chan []byte, icmd btcjson.Cmd) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.DumpWalletCmd)
	if !ok {
		ReplyError(frontend, icmd.Id(), &btcjson.ErrInternal)
		return
	}

	// Iterate over all accounts, appending the private keys
	// for each.
	var keys []string
	for _, a := range accounts.m {
		switch walletKeys, err := a.DumpPrivKeys(); err {
		case wallet.ErrWalletLocked:
			ReplyError(frontend, cmd.Id(), &btcjson.ErrWalletUnlockNeeded)
			return

		case nil:
			keys = append(keys, walletKeys...)

		default: // any other non-nil error
			e := &btcjson.Error{
				Code:    btcjson.ErrWallet.Code,
				Message: err.Error(),
			}
			ReplyError(frontend, cmd.Id(), e)
		}
	}

	// Reply with sorted WIF encoded private keys
	ReplySuccess(frontend, cmd.Id(), keys)
}

// GetAddressesByAccount replies to a getaddressesbyaccount request with
// all addresses for an account, or an error if the requested account does
// not exist.
func GetAddressesByAccount(frontend chan []byte, icmd btcjson.Cmd) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.GetAddressesByAccountCmd)
	if !ok {
		ReplyError(frontend, icmd.Id(), &btcjson.ErrInternal)
		return
	}

	// Check that the account specified in the request exists.
	a, ok := accounts.m[cmd.Account]
	if !ok {
		ReplyError(frontend, cmd.Id(),
			&btcjson.ErrWalletInvalidAccountName)
		return
	}

	// Reply with sorted active payment addresses.
	ReplySuccess(frontend, cmd.Id(), a.SortedActivePaymentAddresses())
}

// GetBalance replies to a getbalance request with the balance for an
// account (wallet), or an error if the requested account does not
// exist.
func GetBalance(frontend chan []byte, icmd btcjson.Cmd) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.GetBalanceCmd)
	if !ok {
		ReplyError(frontend, icmd.Id(), &btcjson.ErrInternal)
		return
	}

	// Check that the account specified in the request exists.
	a, ok := accounts.m[cmd.Account]
	if !ok {
		ReplyError(frontend, cmd.Id(),
			&btcjson.ErrWalletInvalidAccountName)
		return
	}

	// Reply with calculated balance.
	ReplySuccess(frontend, cmd.Id(), a.CalculateBalance(cmd.MinConf))
}

// GetBalances replies to a getbalances extension request by notifying
// the frontend of all balances for each opened account.
func GetBalances(frontend chan []byte, cmd btcjson.Cmd) {
	NotifyBalances(frontend)
}

// ImportPrivKey replies to an importprivkey request by parsing
// a WIF-encoded private key and adding it to an account.
func ImportPrivKey(frontend chan []byte, icmd btcjson.Cmd) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.ImportPrivKeyCmd)
	if !ok {
		ReplyError(frontend, icmd.Id(), &btcjson.ErrInternal)
		return
	}

	// Check that the account specified in the requests exists.
	// Yes, Label is the account name.
	a, ok := accounts.m[cmd.Label]
	if !ok {
		ReplyError(frontend, cmd.Id(),
			&btcjson.ErrWalletInvalidAccountName)
		return
	}

	// Create a blockstamp for when this address first appeared.
	// Because the importprivatekey RPC call does not allow
	// specifying when the address first appeared, we must make
	// a worst case guess.
	bs := &wallet.BlockStamp{Height: 0}

	// Attempt importing the private key, replying with an appropiate
	// error if the import was unsuccesful.
	addr, err := a.ImportWIFPrivateKey(cmd.PrivKey, cmd.Label, bs)
	switch {
	case err == wallet.ErrWalletLocked:
		ReplyError(frontend, cmd.Id(), &btcjson.ErrWalletUnlockNeeded)
		return

	case err != nil:
		e := &btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		ReplyError(frontend, cmd.Id(), e)
		return
	}

	if cmd.Rescan {
		addrs := map[string]struct{}{
			addr: struct{}{},
		}
		a.RescanAddresses(bs.Height, addrs)
	}

	// If the import was successful, reply with nil.
	ReplySuccess(frontend, cmd.Id(), nil)
}

// NotifyBalances notifies an attached frontend of the current confirmed
// and unconfirmed account balances.
//
// TODO(jrick): Switch this to return a JSON object (map) of all accounts
// and their balances, instead of separate notifications for each account.
func NotifyBalances(frontend chan []byte) {
	for _, a := range accounts.m {
		balance := a.CalculateBalance(1)
		unconfirmed := a.CalculateBalance(0) - balance
		NotifyWalletBalance(frontend, a.name, balance)
		NotifyWalletBalanceUnconfirmed(frontend, a.name, unconfirmed)
	}
}

// GetNewAddress responds to a getnewaddress request by getting a new
// address for an account.  If the account does not exist, an appropiate
// error is returned to the frontend.
func GetNewAddress(frontend chan []byte, icmd btcjson.Cmd) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.GetNewAddressCmd)
	if !ok {
		ReplyError(frontend, icmd.Id(), &btcjson.ErrInternal)
		return
	}

	// Check that the account specified in the request exists.
	a, ok := accounts.m[cmd.Account]
	if !ok {
		ReplyError(frontend, cmd.Id(),
			&btcjson.ErrWalletInvalidAccountName)
		return
	}

	// Get next address from wallet.
	addr, err := a.NextUnusedAddress()
	if err != nil {
		// TODO(jrick): generate new addresses if the address pool is
		// empty.
		e := btcjson.ErrInternal
		e.Message = fmt.Sprintf("New address generation not implemented yet")
		ReplyError(frontend, cmd.Id(), &e)
		return
	}

	// Write updated wallet to disk.
	a.dirty = true
	if err = a.writeDirtyToDisk(); err != nil {
		log.Errorf("cannot sync dirty wallet: %v", err)
	}

	// Request updates from btcd for new transactions sent to this address.
	a.ReqNewTxsForAddress(addr)

	// Reply with the new payment address string.
	ReplySuccess(frontend, cmd.Id(), addr)
}

// ListAccounts replies to a listaccounts request by returning a JSON
// object mapping account names with their balances.
func ListAccounts(frontend chan []byte, icmd btcjson.Cmd) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.ListAccountsCmd)
	if !ok {
		ReplyError(frontend, icmd.Id(), &btcjson.ErrInternal)
		return
	}

	// Create and fill a map of account names and their balances.
	pairs := make(map[string]float64)
	for aname, a := range accounts.m {
		pairs[aname] = a.CalculateBalance(cmd.MinConf)
	}

	// Reply with the map.  This will be marshaled into a JSON object.
	ReplySuccess(frontend, cmd.Id(), pairs)
}

// SendFrom creates a new transaction spending unspent transaction
// outputs for a wallet to another payment address.  Leftover inputs
// not sent to the payment address or a fee for the miner are sent
// back to a new address in the wallet.  Upon success, the TxID
// for the created transaction is sent to the frontend.
func SendFrom(frontend chan []byte, icmd btcjson.Cmd) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.SendFromCmd)
	if !ok {
		ReplyError(frontend, icmd.Id(), &btcjson.ErrInternal)
		return
	}

	// Check that signed integer parameters are positive.
	if cmd.Amount < 0 {
		e := &btcjson.Error{
			Code:    btcjson.ErrInvalidParameter.Code,
			Message: "amount must be positive",
		}
		ReplyError(frontend, cmd.Id(), e)
		return
	}
	if cmd.MinConf < 0 {
		e := &btcjson.Error{
			Code:    btcjson.ErrInvalidParameter.Code,
			Message: "minconf must be positive",
		}
		ReplyError(frontend, cmd.Id(), e)
		return
	}

	// Check that the account specified in the request exists.
	a, ok := accounts.m[cmd.FromAccount]
	if !ok {
		ReplyError(frontend, cmd.Id(),
			&btcjson.ErrWalletInvalidAccountName)
		return
	}

	// Create map of address and amount pairs.
	pairs := map[string]int64{
		cmd.ToAddress: cmd.Amount,
	}

	// Get fee to add to tx.
	// TODO(jrick): this needs to be fee per kB.
	TxFee.Lock()
	fee := TxFee.i
	TxFee.Unlock()

	// Create transaction, replying with an error if the creation
	// was not successful.
	createdTx, err := a.txToPairs(pairs, fee, cmd.MinConf)
	switch {
	case err == ErrNonPositiveAmount:
		e := &btcjson.Error{
			Code:    btcjson.ErrInvalidParameter.Code,
			Message: "amount must be positive",
		}
		ReplyError(frontend, cmd.Id(), e)
		return

	case err == wallet.ErrWalletLocked:
		ReplyError(frontend, cmd.Id(), &btcjson.ErrWalletUnlockNeeded)
		return

	case err != nil:
		e := &btcjson.Error{
			Code:    btcjson.ErrInternal.Code,
			Message: err.Error(),
		}
		ReplyError(frontend, cmd.Id(), e)
		return
	}

	// If a change address was added, mark wallet as dirty, sync to disk,
	// and Request updates for change address.
	if len(createdTx.changeAddr) != 0 {
		a.dirty = true
		if err := a.writeDirtyToDisk(); err != nil {
			log.Errorf("cannot write dirty wallet: %v", err)
		}
		a.ReqNewTxsForAddress(createdTx.changeAddr)
	}

	// Create sendrawtransaction request with hexstring of the raw tx.
	n := <-NewJSONID
	var id interface{} = fmt.Sprintf("btcwallet(%v)", n)
	m, err := btcjson.CreateMessageWithId("sendrawtransaction", id,
		hex.EncodeToString(createdTx.rawTx))
	if err != nil {
		e := &btcjson.Error{
			Code:    btcjson.ErrInternal.Code,
			Message: err.Error(),
		}
		ReplyError(frontend, cmd.Id(), e)
		return
	}

	// Set up a reply handler to respond to the btcd reply.
	replyHandlers.Lock()
	replyHandlers.m[n] = func(result interface{}, err *btcjson.Error) bool {
		return handleSendRawTxReply(frontend, cmd, result, err, a,
			createdTx)
	}
	replyHandlers.Unlock()

	// Send sendrawtransaction request to btcd.
	btcdMsgs <- m
}

// SendMany creates a new transaction spending unspent transaction
// outputs for a wallet to any number of  payment addresses.  Leftover
// inputs not sent to the payment address or a fee for the miner are
// sent back to a new address in the wallet.  Upon success, the TxID
// for the created transaction is sent to the frontend.
func SendMany(frontend chan []byte, icmd btcjson.Cmd) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.SendManyCmd)
	if !ok {
		ReplyError(frontend, icmd.Id(), &btcjson.ErrInternal)
		return
	}

	// Check that minconf is positive.
	if cmd.MinConf < 0 {
		e := &btcjson.Error{
			Code:    btcjson.ErrInvalidParameter.Code,
			Message: "minconf must be positive",
		}
		ReplyError(frontend, cmd.Id(), e)
		return
	}

	// Check that the account specified in the request exists.
	a, ok := accounts.m[cmd.FromAccount]
	if !ok {
		ReplyError(frontend, cmd.Id(),
			&btcjson.ErrWalletInvalidAccountName)
		return
	}

	// Get fee to add to tx.
	// TODO(jrick): this needs to be fee per kB.
	TxFee.Lock()
	fee := TxFee.i
	TxFee.Unlock()

	// Create transaction, replying with an error if the creation
	// was not successful.
	createdTx, err := a.txToPairs(cmd.Amounts, fee, cmd.MinConf)
	switch {
	case err == ErrNonPositiveAmount:
		e := &btcjson.Error{
			Code:    btcjson.ErrInvalidParameter.Code,
			Message: "amount must be positive",
		}
		ReplyError(frontend, cmd.Id(), e)
		return

	case err == wallet.ErrWalletLocked:
		ReplyError(frontend, cmd.Id(), &btcjson.ErrWalletUnlockNeeded)
		return

	case err != nil:
		e := &btcjson.Error{
			Code:    btcjson.ErrInternal.Code,
			Message: err.Error(),
		}
		ReplyError(frontend, cmd.Id(), e)
		return
	}

	// If a change address was added, mark wallet as dirty, sync to disk,
	// and request updates for change address.
	if len(createdTx.changeAddr) != 0 {
		a.dirty = true
		if err := a.writeDirtyToDisk(); err != nil {
			log.Errorf("cannot write dirty wallet: %v", err)
		}
		a.ReqNewTxsForAddress(createdTx.changeAddr)
	}

	// Create sendrawtransaction request with hexstring of the raw tx.
	n := <-NewJSONID
	var id interface{} = fmt.Sprintf("btcwallet(%v)", n)
	m, err := btcjson.CreateMessageWithId("sendrawtransaction", id,
		hex.EncodeToString(createdTx.rawTx))
	if err != nil {
		e := &btcjson.Error{
			Code:    btcjson.ErrInternal.Code,
			Message: err.Error(),
		}
		ReplyError(frontend, cmd.Id(), e)
		return
	}

	// Set up a reply handler to respond to the btcd reply.
	replyHandlers.Lock()
	replyHandlers.m[n] = func(result interface{}, err *btcjson.Error) bool {
		return handleSendRawTxReply(frontend, cmd, result, err, a,
			createdTx)
	}
	replyHandlers.Unlock()

	// Send sendrawtransaction request to btcd.
	btcdMsgs <- m
}

func handleSendRawTxReply(frontend chan []byte, icmd btcjson.Cmd,
	result interface{}, err *btcjson.Error, a *Account,
	txInfo *CreatedTx) bool {

	if err != nil {
		ReplyError(frontend, icmd.Id(), err)
		return true
	}

	// Remove previous unspent outputs now spent by the tx.
	a.UtxoStore.Lock()
	modified := a.UtxoStore.s.Remove(txInfo.inputs)

	// Add unconfirmed change utxo (if any) to UtxoStore.
	if txInfo.changeUtxo != nil {
		a.UtxoStore.s = append(a.UtxoStore.s, txInfo.changeUtxo)
		a.ReqSpentUtxoNtfn(txInfo.changeUtxo)
		modified = true
	}

	if modified {
		a.UtxoStore.dirty = true
		a.UtxoStore.Unlock()
		if err := a.writeDirtyToDisk(); err != nil {
			log.Errorf("cannot sync dirty wallet: %v", err)
		}

		// Notify all frontends of account's new unconfirmed and
		// confirmed balance.
		confirmed := a.CalculateBalance(1)
		unconfirmed := a.CalculateBalance(0) - confirmed
		NotifyWalletBalance(frontendNotificationMaster, a.name, confirmed)
		NotifyWalletBalanceUnconfirmed(frontendNotificationMaster, a.name, unconfirmed)
	} else {
		a.UtxoStore.Unlock()
	}

	// btcd cannot be trusted to successfully relay the tx to the
	// Bitcoin network.  Even if this succeeds, the rawtx must be
	// saved and checked for an appearence in a later block. btcd
	// will make a best try effort, but ultimately it's btcwallet's
	// responsibility.
	//
	// Add hex string of raw tx to sent tx pool.  If btcd disconnects
	// and is reconnected, these txs are resent.
	UnminedTxs.Lock()
	UnminedTxs.m[TXID(result.(string))] = txInfo
	UnminedTxs.Unlock()

	log.Debugf("successfully sent transaction %v", result)
	ReplySuccess(frontend, icmd.Id(), result)

	// The comments to be saved differ based on the underlying type
	// of the cmd, so switch on the type to check whether it is a
	// SendFromCmd or SendManyCmd.
	//
	// TODO(jrick): If message succeeded in being sent, save the
	// transaction details with comments.
	switch cmd := icmd.(type) {
	case *btcjson.SendFromCmd:
		_ = cmd.Comment
		_ = cmd.CommentTo

	case *btcjson.SendManyCmd:
		_ = cmd.Comment
	}

	return true
}

// SetTxFee sets the global transaction fee added to transactions.
func SetTxFee(frontend chan []byte, icmd btcjson.Cmd) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.SetTxFeeCmd)
	if !ok {
		ReplyError(frontend, icmd.Id(), &btcjson.ErrInternal)
		return
	}

	// Check that amount is not negative.
	if cmd.Amount < 0 {
		e := &btcjson.Error{
			Code:    btcjson.ErrInvalidParams.Code,
			Message: "amount cannot be negative",
		}
		ReplyError(frontend, cmd.Id(), e)
		return
	}

	// Set global tx fee.
	//
	// TODO(jrick): this must be a fee per kB.
	// TODO(jrick): need to notify all frontends of new tx fee.
	TxFee.Lock()
	TxFee.i = cmd.Amount
	TxFee.Unlock()

	// A boolean true result is returned upon success.
	ReplySuccess(frontend, cmd.Id(), true)
}

// CreateEncryptedWallet creates a new account with an encrypted
// wallet.  If an account with the same name as the requested account
// name already exists, an invalid account name error is returned to
// the client.
//
// Wallets will be created on TestNet3, or MainNet if btcwallet is run with
// the --mainnet option.
func CreateEncryptedWallet(frontend chan []byte, icmd btcjson.Cmd) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcws.CreateEncryptedWalletCmd)
	if !ok {
		ReplyError(frontend, icmd.Id(), &btcjson.ErrInternal)
		return
	}

	// Grab the account map lock and defer the unlock.  If an
	// account is successfully created, it will be added to the
	// map while the lock is held.
	accounts.Lock()
	defer accounts.Unlock()

	// Does this wallet already exist?
	if _, ok = accounts.m[cmd.Account]; ok {
		ReplyError(frontend, cmd.Id(),
			&btcjson.ErrWalletInvalidAccountName)
		return
	}

	// Decide which Bitcoin network must be used.
	var net btcwire.BitcoinNet
	if cfg.MainNet {
		net = btcwire.MainNet
	} else {
		net = btcwire.TestNet3
	}

	// Get current block's height and hash.
	bs, err := GetCurBlock()
	if err != nil {
		e := &btcjson.Error{
			Code:    btcjson.ErrInternal.Code,
			Message: "btcd disconnected",
		}
		ReplyError(frontend, cmd.Id(), e)
		return
	}

	// Create new wallet in memory.
	wlt, err := wallet.NewWallet(cmd.Account, cmd.Description,
		[]byte(cmd.Passphrase), net, &bs)
	if err != nil {
		log.Error("Error creating wallet: " + err.Error())
		ReplyError(frontend, cmd.Id(), &btcjson.ErrInternal)
		return
	}

	// Create new account with the wallet.  A new JSON ID is set for
	// transaction notifications.
	a := &Account{
		Wallet:         wlt,
		name:           cmd.Account,
		dirty:          true,
		NewBlockTxSeqN: <-NewJSONID,
	}

	// Begin tracking account against a connected btcd.
	//
	// TODO(jrick): this should *only* happen if btcd is connected.
	a.Track()

	// Save the account in the global account map.  The mutex is
	// already held at this point, and will be unlocked when this
	// func returns.
	accounts.m[cmd.Account] = a

	// Write new wallet to disk.
	if err := a.writeDirtyToDisk(); err != nil {
		log.Errorf("cannot sync dirty wallet: %v", err)
	}

	// Notify all frontends of this new account, and its balance.
	NotifyBalances(frontendNotificationMaster)

	// A nil reply is sent upon successful wallet creation.
	ReplySuccess(frontend, cmd.Id(), nil)
}

// WalletIsLocked responds to the walletislocked extension request by
// replying with the current lock state (false for unlocked, true for
// locked) of an account.  An error is returned if the requested account
// does not exist.
func WalletIsLocked(frontend chan []byte, icmd btcjson.Cmd) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcws.WalletIsLockedCmd)
	if !ok {
		ReplyError(frontend, icmd.Id(), &btcjson.ErrInternal)
		return
	}

	// Check that the account specified in the request exists.
	a, ok := accounts.m[cmd.Account]
	if !ok {
		ReplyError(frontend, cmd.Id(),
			&btcjson.ErrWalletInvalidAccountName)
		return
	}

	// Reply with true for a locked wallet, and false for unlocked.
	ReplySuccess(frontend, cmd.Id(), a.IsLocked())
}

// WalletLock responds to walletlock request by locking the wallet,
// replying with an error if the wallet is already locked.
//
// TODO(jrick): figure out how multiple wallets/accounts will work
// with this.  Lock all the wallets, like if all accounts are locked
// for one bitcoind wallet?
func WalletLock(frontend chan []byte, icmd btcjson.Cmd) {
	if a, ok := accounts.m[""]; ok {
		if err := a.Lock(); err != nil {
			ReplyError(frontend, icmd.Id(),
				&btcjson.ErrWalletWrongEncState)
			return
		}
		ReplySuccess(frontend, icmd.Id(), nil)
		NotifyWalletLockStateChange("", true)
	}
}

// WalletPassphrase responds to the walletpassphrase request by unlocking
// the wallet.  The decryption key is saved in the wallet until timeout
// seconds expires, after which the wallet is locked.
//
// TODO(jrick): figure out how to do this for non-default accounts.
func WalletPassphrase(frontend chan []byte, icmd btcjson.Cmd) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.WalletPassphraseCmd)
	if !ok {
		ReplyError(frontend, icmd.Id(), &btcjson.ErrInternal)
		return
	}

	if a, ok := accounts.m[""]; ok {
		if err := a.Unlock([]byte(cmd.Passphrase)); err != nil {
			ReplyError(frontend, cmd.Id(),
				&btcjson.ErrWalletPassphraseIncorrect)
			return
		}
		ReplySuccess(frontend, cmd.Id(), nil)
		NotifyWalletLockStateChange("", false)
		go func() {
			time.Sleep(time.Second * time.Duration(int64(cmd.Timeout)))
			a.Lock()
			NotifyWalletLockStateChange("", true)
		}()
	}
}

// AccountNtfn is a struct for marshalling any generic notification
// about a account for a wallet frontend.
//
// TODO(jrick): move to btcjson so it can be shared with frontends?
type AccountNtfn struct {
	Account      string      `json:"account"`
	Notification interface{} `json:"notification"`
}

// NotifyWalletLockStateChange sends a notification to all frontends
// that the wallet has just been locked or unlocked.
func NotifyWalletLockStateChange(account string, locked bool) {
	var id interface{} = "btcwallet:newwalletlockstate"
	m := btcjson.Reply{
		Result: &AccountNtfn{
			Account:      account,
			Notification: locked,
		},
		Id: &id,
	}
	msg, _ := json.Marshal(&m)
	frontendNotificationMaster <- msg
}

// NotifyWalletBalance sends a confirmed account balance notification
// to a frontend.
func NotifyWalletBalance(frontend chan []byte, account string, balance float64) {
	var id interface{} = "btcwallet:accountbalance"
	m := btcjson.Reply{
		Result: &AccountNtfn{
			Account:      account,
			Notification: balance,
		},
		Id: &id,
	}
	msg, _ := json.Marshal(&m)
	frontend <- msg
}

// NotifyWalletBalanceUnconfirmed  sends a confirmed account balance
// notification to a frontend.
func NotifyWalletBalanceUnconfirmed(frontend chan []byte, account string, balance float64) {
	var id interface{} = "btcwallet:accountbalanceunconfirmed"
	m := btcjson.Reply{
		Result: &AccountNtfn{
			Account:      account,
			Notification: balance,
		},
		Id: &id,
	}
	msg, _ := json.Marshal(&m)
	frontend <- msg
}
