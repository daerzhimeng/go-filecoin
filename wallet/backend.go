package wallet

import (
	"github.com/filecoin-project/go-filecoin/types"
)

// Backend is the interface to represent different storage backends
// that can contain many addresses.
type Backend interface {
	// Addresses returns a list of all accounts currently stored in this backend.
	Addresses() []types.Address

	// Contains returns true if this backend stores the passed in address.
	HasAddress(addr types.Address) bool

	// Sign cryptographically signs `data` using the private key `priv`.
	SignBytes(data []byte, addr types.Address) (types.Signature, error)

	// Verify cryptographically verifies that 'sig' is the signed hash of 'data' with
	// the public key `pk`.
	Verify(data []byte, pk []byte, sig types.Signature) (bool, error)

	// Ecrecover returns an uncompressed public key that could produce the given
	// signature from data.
	// Note: The returned public key should not be used to verify `data` is valid
	// since a public key may have N private key pairs
	Ecrecover(data []byte, sig types.Signature) ([]byte, error)

	// GetKeyInfo will return the keyinfo associated with address `addr`
	// iff backend contains the addr.
	GetKeyInfo(addr types.Address) (*types.KeyInfo, error)
}

// Importer is a specialization of a wallet backend that can import
// new keys into its permanent storage. Disk backed wallets can do this,
// hardware wallets generally cannot.
type Importer interface {
	// ImportKey imports the key described by the given keyinfo
	// into the backend
	ImportKey(ki *types.KeyInfo) error
}
