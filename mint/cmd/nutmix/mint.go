package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lescuer97/nutmix/api/cashu"
	"github.com/lescuer97/nutmix/internal/comms"
	"github.com/lescuer97/nutmix/pkg/crypto"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/tyler-smith/go-bip32"
)

type KeysetMap map[uint64]cashu.Keyset

type Mint struct {
	ActiveKeysets map[string]KeysetMap
	Keysets       map[string][]cashu.Keyset
	LightningComs comms.LightingComms
	Network       chaincfg.Params
	PendingProofs []cashu.Proof
}

// errors types for validation

var (
	ErrKeysetNotFound         = errors.New("Keyset not found")
	ErrKeysetForProofNotFound = errors.New("Keyset for proof not found")
	ErrInvalidProof           = errors.New("Invalid proof")
	ErrQuoteNotPaid           = errors.New("Quote not paid")
    ErrMessageAmountToBig     = errors.New("Message amount is to big")
    ErrInvalidBlindMessage    = errors.New("Invalid blind message")
)

func(m *Mint) CheckProofsAreSameUnit( proofs []cashu.Proof) (cashu.Unit, error) {


    units := make(map[string]bool)


    for _, proof := range proofs {
         

        keyset, err := m.GetKeysetById(proof.Id)
        if err != nil {
            return cashu.Sat, fmt.Errorf("GetKeysetById: %w", err)
        }

        if len(keyset) == 0 {
            return cashu.Sat, ErrKeysetForProofNotFound
        }

        units[keyset[0].Unit] = true
        if len(units) > 1 {
            return cashu.Sat, fmt.Errorf("Proofs are not the same unit")
        }
    }

    if len(units) == 0 {
        return cashu.Sat, fmt.Errorf("No units found")
    }

    var returnedUnit cashu.Unit
    for unit := range units {

        finalUnit, err := cashu.UnitFromString(unit)
        if err != nil {
            return cashu.Sat, fmt.Errorf("UnitFromString: %w", err)
        }

        returnedUnit = finalUnit
    }

    return returnedUnit, nil

}

func (m *Mint) ValidateProof(proof cashu.Proof, unit cashu.Unit) error {
	var keysetToUse cashu.Keyset
	for _, keyset := range m.Keysets[unit.String()] {
		if keyset.Amount == proof.Amount && keyset.Id == proof.Id {
			keysetToUse = keyset
			break
		}
	}

	// check if keysetToUse is not assigned
	if keysetToUse.Id == "" {
		return ErrKeysetForProofNotFound
	}

	parsedBlinding, err := hex.DecodeString(proof.C)

	if err != nil {
		log.Printf("hex.DecodeString: %+v", err)
		return err
	}

	pubkey, err := secp256k1.ParsePubKey(parsedBlinding)
	if err != nil {
		log.Printf("secp256k1.ParsePubKey: %+v", err)
		return err
	}

	verified := crypto.Verify(proof.Secret, keysetToUse.PrivKey, pubkey)

	if !verified {
		return ErrInvalidProof
	}

	return nil
}

func (m *Mint) SignBlindedMessages(outputs []cashu.BlindedMessage, unit string) ([]cashu.BlindSignature, error) {
	var blindedSignatures []cashu.BlindSignature

	for _, output := range outputs {

		correctKeyset := m.ActiveKeysets[unit][output.Amount]

		blindSignature, err := output.GenerateBlindSignature(correctKeyset.PrivKey)

		if err != nil {
            err = fmt.Errorf("GenerateBlindSignature: %w %w",ErrInvalidBlindMessage, err)
			return nil, err
		}

		blindedSignatures = append(blindedSignatures, blindSignature)

	}
	return blindedSignatures, nil
}

func (m *Mint) GetKeysetById( id string) ([]cashu.Keyset, error) {

    allKeys := m.GetAllKeysets()
	var keyset []cashu.Keyset

	for _, key := range allKeys {

		if key.Id == id {
			keyset = append(keyset, key)
		}
	}

	return keyset, nil
}

func (m *Mint) GetAllKeysets() []cashu.Keyset {
    var allKeys []cashu.Keyset

    for _, keyset := range m.Keysets {
        allKeys = append(allKeys, keyset...)
    }

    return allKeys
}

func (m *Mint) OrderActiveKeysByUnit() cashu.KeysResponse {
	// convert map to slice
	var keys []cashu.Keyset
	for _, keyset := range m.ActiveKeysets {
		for _, key := range keyset {
			keys = append(keys, key)
		}
	}

	orderedKeys := cashu.OrderKeysetByUnit(keys)

	return orderedKeys
}

func SetUpMint(seeds []cashu.Seed) (Mint, error) {
	mint := Mint{
		ActiveKeysets: make(map[string]KeysetMap),
		Keysets:       make(map[string][]cashu.Keyset),
	}

	network := os.Getenv("NETWORK")
	switch network {
	case "testnet":
		mint.Network = chaincfg.TestNet3Params
	case "mainnet":
		mint.Network = chaincfg.MainNetParams
	case "regtest":
		mint.Network = chaincfg.RegressionNetParams
	case "signet":
		mint.Network = chaincfg.SigNetParams
	default:
		return mint, fmt.Errorf("Invalid network: %s", network)
	}

	lightningBackendType := os.Getenv("MINT_LIGHTNING_BACKEND")
	switch lightningBackendType {

	case comms.FAKE_WALLET:

	case comms.LND_WALLET:
		lightningComs, err := comms.SetupLightingComms()

		if err != nil {
			return mint, err
		}
		mint.LightningComs = *lightningComs
	default:
		log.Fatalf("Unknown lightning backend: %s", lightningBackendType)
	}

	mint.PendingProofs = make([]cashu.Proof, 0)

	// uses seed to generate the keysets
	for _, seed := range seeds {
		masterKey, err := bip32.NewMasterKey(seed.Seed)
		if err != nil {
			log.Println(fmt.Errorf("NewMasterKey: %v", err))
			return mint, err
		}

		unit, err := cashu.UnitFromString(seed.Unit)
		if err != nil {
			log.Println(fmt.Errorf("cashu.UnitFromString: %v", err))
			return mint, err
		}

		keysets, err := cashu.GenerateKeysets(masterKey, cashu.GetAmountsForKeysets(), seed.Id, unit)

		if err != nil {
			return mint, fmt.Errorf("GenerateKeysets: %v", err)
		}

		if seed.Active {
			mint.ActiveKeysets[seed.Unit] = make(KeysetMap)
			for _, keyset := range keysets {
				mint.ActiveKeysets[seed.Unit][keyset.Amount] = keyset
			}

		}

		mint.Keysets[seed.Unit] = append(mint.Keysets[seed.Unit], keysets...)
	}

	return mint, nil
}



type AddToDBFunc func(*pgxpool.Pool, bool, string) error

func (m *Mint) VerifyLightingPaymentHappened(pool *pgxpool.Pool, paid bool, quote string, dbCall AddToDBFunc) (bool, error) {
	lightningBackendType := os.Getenv("MINT_LIGHTNING_BACKEND")
	switch lightningBackendType {

	case comms.FAKE_WALLET:
		err := dbCall(pool, true, quote)
		if err != nil {
			return false, fmt.Errorf("dbCall: %w", err)
		}

		return true, nil

	case comms.LND_WALLET:
		invoiceDB, err := m.LightningComs.CheckIfInvoicePayed(quote)
		if err != nil {
			return false, fmt.Errorf("mint.LightningComs.CheckIfInvoicePayed: %w", err)
		}
		if invoiceDB.State == lnrpc.Invoice_SETTLED {
			err := dbCall(pool, true, quote)
			if err != nil {
				return false, fmt.Errorf("dbCall: %w", err)
			}
			return true, nil

		} else {

			return false, nil
		}

	}
	return false, nil
}
