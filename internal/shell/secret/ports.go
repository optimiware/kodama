package secret

// This package declares no effectful secret port.
//
// Secret custody — encryption keys, envelope associated data and the concrete
// key adapters — is an adapter contract. Stored-secret reads expose metadata
// only and writes commit through the operation registry; activation data
// materialization is a Kernel preparation seam composed by the Shell. Declaring
// a second custody port here would duplicate that contract and risk exposing
// plaintext outside the authorized delivery path.
