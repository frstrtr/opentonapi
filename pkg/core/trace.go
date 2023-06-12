package core

import (
	"context"

	"github.com/tonkeeper/tongo"
	"github.com/tonkeeper/tongo/abi"
)

type Trace struct {
	// Transaction is slightly modified.
	// For example, we have kept only external outbound messages in OutMsgs.
	Transaction
	AccountInterfaces []abi.ContractInterface
	Children          []*Trace
	AdditionalInfo    *TraceAdditionalInfo
}

// TraceAdditionalInfo holds information about a trace
// but not directly extracted from it or a corresponding transaction.
type TraceAdditionalInfo struct {
	JettonMaster *tongo.AccountID
	// NftSaleContract is set, if a transaction's account implements "get_sale_data" method.
	NftSaleContract *NftSaleContract
}

func (t *Trace) InProgress() bool {
	return t.countUncompleted() != 0
}
func (t *Trace) countUncompleted() int {
	c := len(t.OutMsgs) //todo: not count externals
	for _, st := range t.Children {
		c += st.countUncompleted()
	}
	return c
}

// NftSaleContract holds partial results of get_sale_data method.
type NftSaleContract struct {
	NftPrice int64
	// Owner of an NFT according to a getgems/basic contract.
	Owner *tongo.AccountID
}

// InformationSource provides methods to construct TraceAdditionalInfo.
type InformationSource interface {
	JettonMastersForWallets(ctx context.Context, wallets []tongo.AccountID) (map[tongo.AccountID]tongo.AccountID, error)
	GetGemsContracts(ctx context.Context, getGems []tongo.AccountID) (map[tongo.AccountID]NftSaleContract, error)
	NftSaleContracts(ctx context.Context, contracts []tongo.AccountID) (map[tongo.AccountID]NftSaleContract, error)
}

func isDestinationJettonWallet(inMsg *Message) bool {
	if inMsg == nil || inMsg.DecodedBody == nil {
		return false
	}
	return inMsg.DecodedBody.Operation == abi.JettonTransferMsgOp && inMsg.Destination != nil
}

func hasInterface(interfacesList []abi.ContractInterface, name abi.ContractInterface) bool {
	for _, iface := range interfacesList {
		if iface == name {
			return true
		}
	}
	return false
}

func visit(trace *Trace, fn func(trace *Trace)) {
	fn(trace)
	for _, child := range trace.Children {
		visit(child, fn)
	}
}

// CollectAdditionalInfo goes over the whole trace
// and populates trace.TraceAdditionalInfo based on information
// provided by InformationSource.
func CollectAdditionalInfo(ctx context.Context, infoSource InformationSource, trace *Trace) error {
	if infoSource == nil {
		return nil
	}
	var jettonWallets []tongo.AccountID
	var getGemsContracts []tongo.AccountID
	var basicNftSale []tongo.AccountID
	visit(trace, func(trace *Trace) {
		if isDestinationJettonWallet(trace.InMsg) {
			jettonWallets = append(jettonWallets, *trace.InMsg.Destination)
		}
		if hasInterface(trace.AccountInterfaces, abi.NftSaleGetgems) {
			getGemsContracts = append(getGemsContracts, trace.Account)
		}
		if hasInterface(trace.AccountInterfaces, abi.NftSale) {
			basicNftSale = append(basicNftSale, trace.Account)
		}
	})
	masters, err := infoSource.JettonMastersForWallets(ctx, jettonWallets)
	if err != nil {
		return err
	}
	getGems, err := infoSource.GetGemsContracts(ctx, getGemsContracts)
	if err != nil {
		return err
	}
	basicNftSales, err := infoSource.NftSaleContracts(ctx, basicNftSale)
	if err != nil {
		return err
	}
	visit(trace, func(trace *Trace) {
		trace.AdditionalInfo = &TraceAdditionalInfo{}
		if isDestinationJettonWallet(trace.InMsg) {
			if master, ok := masters[*trace.InMsg.Destination]; ok {
				trace.AdditionalInfo.JettonMaster = &master
			}
		}
		if hasInterface(trace.AccountInterfaces, abi.NftSaleGetgems) {
			if getgems, ok := getGems[trace.Account]; ok {
				trace.AdditionalInfo.NftSaleContract = &getgems
			}
		}
		if hasInterface(trace.AccountInterfaces, abi.NftSale) {
			if sale, ok := basicNftSales[trace.Account]; ok {
				trace.AdditionalInfo.NftSaleContract = &sale
			}
		}
	})
	return nil
}
