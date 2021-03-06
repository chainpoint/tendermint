package blockchain

import (
	amino "github.com/tendermint/go-amino"
	"github.com/chainpoint/tendermint/types"
)

var cdc = amino.NewCodec()

func init() {
	RegisterBlockchainMessages(cdc)
	types.RegisterBlockAmino(cdc)
}
