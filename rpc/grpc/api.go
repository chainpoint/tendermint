package core_grpc

import (
	"context"

	abci "github.com/chainpoint/tendermint/abci/types"
	core "github.com/chainpoint/tendermint/rpc/core"
	rpctypes "github.com/chainpoint/tendermint/rpc/lib/types"
)

type broadcastAPI struct {
}

func (bapi *broadcastAPI) Ping(ctx context.Context, req *RequestPing) (*ResponsePing, error) {
	// kvstore so we can check if the server is up
	return &ResponsePing{}, nil
}

func (bapi *broadcastAPI) BroadcastTx(ctx context.Context, req *RequestBroadcastTx) (*ResponseBroadcastTx, error) {
	// NOTE: there's no way to get client's remote address
	// see https://stackoverflow.com/questions/33684570/session-and-remote-ip-address-in-grpc-go
	res, err := core.BroadcastTxCommit(&rpctypes.Context{}, req.Tx)
	if err != nil {
		return nil, err
	}

	return &ResponseBroadcastTx{
		CheckTx: &abci.ResponseCheckTx{
			Code: res.CheckTx.Code,
			Data: res.CheckTx.Data,
			Log:  res.CheckTx.Log,
		},
		DeliverTx: &abci.ResponseDeliverTx{
			Code: res.DeliverTx.Code,
			Data: res.DeliverTx.Data,
			Log:  res.DeliverTx.Log,
		},
	}, nil
}

func (bapi *broadcastAPI) BroadcastMsg(ctx context.Context, req *RequestBroadcastMsg) (*ResponseBroadcastMsg, error) {
	// NOTE: there's no way to get client's remote address
	// see https://stackoverflow.com/questions/33684570/session-and-remote-ip-address-in-grpc-go
	res, err := core.BroadcastMsgCommit(&rpctypes.Context{}, req.Msg)
	if err != nil {
		return nil, err
	}

	return &ResponseBroadcastMsg{
		DeliverMsg: &abci.ResponseDeliverMsg{
			Code: res.DeliverMsg.Code,
			Data: res.DeliverMsg.Data,
			Log:  res.DeliverMsg.Log,
		},
	}, nil
}
