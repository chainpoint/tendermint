package node

import (
	amino "github.com/tendermint/go-amino"
	cryptoamino "github.com/chainpoint/tendermint/crypto/encoding/amino"
)

var cdc = amino.NewCodec()

func init() {
	cryptoamino.RegisterAmino(cdc)
}
