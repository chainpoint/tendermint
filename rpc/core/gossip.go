package core

import (
	"fmt"
	abci "github.com/chainpoint/tendermint/abci/types"
	ctypes "github.com/chainpoint/tendermint/rpc/core/types"
	"github.com/chainpoint/tendermint/rpc/lib/types"
	"github.com/chainpoint/tendermint/types"
)

func BroadcastMsgAsync(ctx *rpctypes.Context, tx types.Tx) (*ctypes.ResultBroadcastMsg, error) {
	err := gossip.DeliverMsg(tx, nil)
	if err != nil {
		return nil, err
	}
	return &ctypes.ResultBroadcastMsg{Hash: tx.Hash()}, nil
}


func BroadcastMsgSync(ctx *rpctypes.Context, tx types.Tx) (*ctypes.ResultBroadcastMsg, error) {
	resCh := make(chan *abci.Response, 1)
	err := gossip.DeliverMsg(tx, func(res *abci.Response) {
		resCh <- res
	})
	if err != nil {
		return nil, err
	}
	res := <-resCh
	r := res.GetCheckTx()
	return &ctypes.ResultBroadcastMsg{
		Code: r.Code,
		Data: r.Data,
		Log:  r.Log,
		Hash: tx.Hash(),
	}, nil
}


func BroadcastMsgCommit(ctx *rpctypes.Context, tx types.Tx) (*ctypes.ResultBroadcastMsgCommit, error) {
	subscriber := ctx.RemoteAddr()

	if eventBus.NumClients() >= config.MaxSubscriptionClients {
		return nil, fmt.Errorf("max_subscription_clients %d reached", config.MaxSubscriptionClients)
	} else if eventBus.NumClientSubscriptions(subscriber) >= config.MaxSubscriptionsPerClient {
		return nil, fmt.Errorf("max_subscriptions_per_client %d reached", config.MaxSubscriptionsPerClient)
	}

	// Broadcast tx and wait for CheckTx result
	checkMsgResCh := make(chan *abci.Response, 1)
	err := gossip.DeliverMsg(tx, func(res *abci.Response) {
		checkMsgResCh <- res
	})
	if err != nil {
		logger.Error("Error on broadcastMsgCommit", "err", err)
		return nil, fmt.Errorf("Error on broadcastMsgCommit: %v", err)
	}
	checkMsgResMsg := <-checkMsgResCh
	checkMsgRes := checkMsgResMsg.GetDeliverMsg()
	return &ctypes.ResultBroadcastMsgCommit{
		DeliverMsg: *checkMsgRes,
		Hash:      tx.Hash(),
	}, nil
}
