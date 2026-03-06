package x402avm

import (
	"fmt"
	"strings"

	x402 "github.com/GoPlausible/x402-avm/go"
)

// NetworkInfo holds the CAIP-2 network identifier and chain family
// for a supported blockchain network.
type NetworkInfo struct {
	// CAIP2 is the canonical network identifier (e.g. "algorand:wGHE2Pw...")
	CAIP2 string
	// ChainFamily is "avm", "evm", or "svm"
	ChainFamily string
}

// knownNetworks maps human-friendly short names to network metadata.
// Both short names and raw CAIP-2 strings are accepted by resolveNetwork().
//
// Supported short names:
//
//	algorand-mainnet  algorand-testnet
//	solana-mainnet    solana-devnet
//	base              base-sepolia
var knownNetworks = map[string]NetworkInfo{
	"algorand-mainnet": {
		CAIP2:       "algorand:wGHE2Pwdvd7S12BL5FaOP20EGYesN73ktiC1qzkkit8=",
		ChainFamily: "avm",
	},
	"algorand-testnet": {
		CAIP2:       "algorand:SGO1GKSzyE7IEPItTxCByw9x8FmnrCDexi9/cOUJOiI=",
		ChainFamily: "avm",
	},
	"solana-mainnet": {
		CAIP2:       "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp",
		ChainFamily: "svm",
	},
	"solana-devnet": {
		CAIP2:       "solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1",
		ChainFamily: "svm",
	},
	"base": {
		CAIP2:       "eip155:8453",
		ChainFamily: "evm",
	},
	"base-sepolia": {
		CAIP2:       "eip155:84532",
		ChainFamily: "evm",
	},
}

// caip2ToFamily maps CAIP-2 namespace prefixes to chain families,
// used when the caller passes a raw CAIP-2 string.
var caip2ToFamily = map[string]string{
	"algorand": "avm",
	"eip155":   "evm",
	"solana":   "svm",
}

// resolveNetwork accepts either a short name ("algorand-mainnet") or a raw
// CAIP-2 string ("algorand:wGHE2...") and returns the canonical x402.Network
// value and the chain family string ("avm"/"evm"/"svm").
func resolveNetwork(name string) (x402.Network, string, error) {
	// Short name lookup.
	if info, ok := knownNetworks[name]; ok {
		return x402.Network(info.CAIP2), info.ChainFamily, nil
	}

	// Accept raw CAIP-2 strings – derive chain family from the namespace.
	if idx := strings.IndexByte(name, ':'); idx > 0 {
		ns := name[:idx]
		if family, ok := caip2ToFamily[ns]; ok {
			return x402.Network(name), family, nil
		}
	}

	return "", "", fmt.Errorf(
		"unknown network %q – use a short name (e.g. algorand-mainnet, solana-mainnet, base) or a CAIP-2 string",
		name,
	)
}
