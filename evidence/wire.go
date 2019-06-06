package evidence

import (
	amino "github.com/tendermint/go-amino"
	cryptoAmino "github.com/chainpoint/tendermint/crypto/encoding/amino"
	"github.com/chainpoint/tendermint/types"
)

var cdc = amino.NewCodec()

func init() {
	RegisterEvidenceMessages(cdc)
	cryptoAmino.RegisterAmino(cdc)
	types.RegisterEvidences(cdc)
}

// For testing purposes only
func RegisterMockEvidences() {
	types.RegisterMockEvidences(cdc)
}
