package state

import (
	amino "github.com/tendermint/go-amino"
	cryptoAmino "github.com/chainpoint/tendermint/crypto/encoding/amino"
)

var cdc = amino.NewCodec()

func init() {
	cryptoAmino.RegisterAmino(cdc)
}
