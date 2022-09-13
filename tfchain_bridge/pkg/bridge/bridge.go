package bridge

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	hProtocol "github.com/stellar/go/protocols/horizon"
	"github.com/threefoldtech/substrate-client"
	"github.com/threefoldtech/tfchain_bridge/pkg"
	"github.com/threefoldtech/tfchain_bridge/pkg/stellar"
	subpkg "github.com/threefoldtech/tfchain_bridge/pkg/substrate"
)

const (
	BridgeNetwork = "stellar"
)

// Bridge is a high lvl structure which listens on contract events and bridge-related
// stellar transactions, and handles them
type Bridge struct {
	wallet           *stellar.StellarWallet
	subClient        *subpkg.SubstrateClient
	blockPersistency *pkg.ChainPersistency
	mut              sync.Mutex
	config           *pkg.BridgeConfig
	depositFee       int64
}

func NewBridge(ctx context.Context, cfg pkg.BridgeConfig) (*Bridge, error) {
	subClient, err := subpkg.NewSubstrateClient(cfg.TfchainURL, cfg.TfchainSeed)
	if err != nil {
		return nil, err
	}

	isValidator, err := subClient.IsValidator(subClient.Identity)
	if err != nil {
		return nil, err
	}

	if !isValidator {
		return nil, fmt.Errorf("account provided is not a validator for the bridge runtime")
	}

	blockPersistency, err := pkg.InitPersist(cfg.PersistencyFile)
	if err != nil {
		return nil, err
	}

	wallet, err := stellar.NewStellarWallet(ctx, &cfg.StellarConfig)
	if err != nil {
		return nil, err
	}

	if cfg.RescanBridgeAccount {
		// saving the cursor to 0 will trigger the bridge stellar account
		// to scan for every transaction ever made on the bridge account
		// and mint accordingly
		err = blockPersistency.SaveStellarCursor("0")
		if err != nil {
			return nil, err
		}
		err = blockPersistency.SaveHeight(0)
		if err != nil {
			return nil, err
		}
	}

	// fetch the configured depositfee
	depositFee, err := subClient.GetDepositFee(subClient.Identity)
	if err != nil {
		return nil, err
	}

	bridge := &Bridge{
		subClient:        subClient,
		blockPersistency: blockPersistency,
		wallet:           wallet,
		config:           &cfg,
		depositFee:       depositFee,
	}

	return bridge, nil
}

func (bridge *Bridge) Start(ctx context.Context) error {
	height, err := bridge.blockPersistency.GetHeight()
	if err != nil {
		return errors.Wrap(err, "failed to get block height from persistency")
	}
	log.Info().Msg("starting stellar subscription...")
	mintChan, err := bridge.wallet.MonitorBridgeAccountAndMint(ctx, height.StellarCursor)
	if err != nil {
		return errors.Wrap(err, "failed to monitor bridge account")
	}

	log.Info().Msg("starting tfchain subscription...")
	tfchainBridgeSub, err := bridge.subClient.SubscribeTfchainBridgeEvents(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to subscribe to tfchain")
	}

	for {
		select {
		case data := <-tfchainBridgeSub:
			if data.Err != nil {
				return errors.Wrap(err, "failed to process events")
			}
			for _, withdrawCreatedEvent := range data.Events.WithdrawCreatedEvents {
				err := bridge.handleWithdrawCreated(ctx, withdrawCreatedEvent)
				if err != nil {
					return errors.Wrap(err, "failed to handle withdraw created")
				}
			}
			for _, withdrawExpiredEvent := range data.Events.WithdrawExpiredEvents {
				err := bridge.handleWithdrawExpired(ctx, withdrawExpiredEvent)
				if err != nil {
					return errors.Wrap(err, "failed to handle withdraw expired")
				}
			}
			for _, withdawReadyEvent := range data.Events.WithdrawReadyEvents {
				err := bridge.handleWithdrawReady(ctx, withdawReadyEvent)
				if err != nil {
					return errors.Wrap(err, "failed to handle withdraw ready")
				}
			}
			for _, refundReadyEvent := range data.Events.RefundReadyEvents {
				err := bridge.handleRefundReady(ctx, refundReadyEvent)
				if err != nil {
					return errors.Wrap(err, "failed to handle refund ready")
				}
			}
			for _, refundExpiredEvent := range data.Events.RefundExpiredEvents {
				err := bridge.handleRefundExpired(ctx, refundExpiredEvent)
				if err != nil {
					return errors.Wrap(err, "failed to handle refund expired")
				}
			}
		case mintEvent := <-mintChan:
			err := bridge.mint(mintEvent.Senders, mintEvent.Tx)
			for err != nil {
				log.Err(err).Msg("Error occured while minting")
				if errors.Is(err, pkg.ErrTransactionAlreadyRefunded) {
					continue
				}

				select {
				case <-ctx.Done():
					return err
				case <-time.After(10 * time.Second):
					err = bridge.mint(mintEvent.Senders, mintEvent.Tx)
				}
			}
			if err != nil {
				return errors.Wrap(err, "failed to handle mint")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// mint handler for stellar
func (bridge *Bridge) mint(senders map[string]*big.Int, tx hProtocol.Transaction) error {
	log.Info().Msg("calling mint now")

	if len(senders) > 1 {
		log.Info().Msgf("cannot process mint transaction, multiple senders found, refunding now")
		for sender, depositAmount := range senders {
			return bridge.refund(context.Background(), sender, depositAmount.Int64(), tx)
		}
	}

	var receiver string
	var depositedAmount *big.Int
	for receiv, amount := range senders {
		receiver = receiv
		depositedAmount = amount
	}

	if tx.Memo == "" {
		log.Info().Msgf("transaction with hash %s has empty memo, refunding now", tx.Hash)
		return bridge.refund(context.Background(), receiver, depositedAmount.Int64(), tx)
	}

	if tx.MemoType == "return" {
		log.Debug().Msgf("transaction with hash %s has a return memo hash, skipping this transaction", tx.Hash)
		// save cursor
		cursor := tx.PagingToken()
		err := bridge.blockPersistency.SaveStellarCursor(cursor)
		if err != nil {
			log.Err(err).Msgf("error while saving cursor")
			return err
		}
		log.Info().Msg("stellar cursor saved")
		return nil
	}

	// TODO check if we already minted for this txid
	minted, err := bridge.subClient.IsMintedAlready(bridge.subClient.Identity, tx.Hash)
	if err != nil && err != substrate.ErrMintTransactionNotFound {
		return err
	}

	if minted {
		log.Error().Msgf("transaction with hash %s is already minted", tx.Hash)
		return nil
	}

	// if the deposited amount is lower than the depositfee, trigger a refund
	if depositedAmount.Cmp(big.NewInt(bridge.depositFee)) <= 0 {
		return bridge.refund(context.Background(), receiver, depositedAmount.Int64(), tx)
	}

	destinationSubstrateAddress, err := bridge.getSubstrateAddressFromMemo(tx.Memo)
	if err != nil {
		log.Err(err).Msgf("error while decoding tx memo")
		// memo is not formatted correctly, issue a refund
		return bridge.refund(context.Background(), receiver, depositedAmount.Int64(), tx)
	}

	log.Info().Int64("amount", depositedAmount.Int64()).Str("tx_id", tx.Hash).Msgf("target substrate address to mint on: %s", destinationSubstrateAddress)

	accountID, err := substrate.FromAddress(destinationSubstrateAddress)
	if err != nil {
		return err
	}

	call, err := bridge.subClient.ProposeOrVoteMintTransaction(bridge.subClient.Identity, tx.Hash, accountID, depositedAmount)
	if err != nil {
		return err
	}

	hash, err := bridge.subClient.CallExtrinsic(call)
	if err != nil {
		return err
	}
	log.Debug().Msgf("mint call submitted with hash: %s", hash.Hex())

	log.Info().Msg("Mint succesfull, saving cursor now")
	// save cursor
	cursor := tx.PagingToken()
	err = bridge.blockPersistency.SaveStellarCursor(cursor)
	if err != nil {
		log.Err(err).Msgf("error while saving cursor")
		return err
	}

	return nil
}

// refund handler for stellar
func (bridge *Bridge) refund(ctx context.Context, destination string, amount int64, tx hProtocol.Transaction) error {
	err := bridge.handleRefundExpired(ctx, subpkg.RefundTransactionExpiredEvent{
		Hash:   tx.Hash,
		Amount: uint64(amount),
		Target: destination,
	})
	if err != nil {
		return err
	}

	// save cursor
	cursor := tx.PagingToken()
	log.Info().Msgf("saving cursor now %s", cursor)
	err = bridge.blockPersistency.SaveStellarCursor(cursor)
	if err != nil {
		log.Error().Msgf("error while saving cursor:", err.Error())
		return err
	}
	return nil
}

func (bridge *Bridge) handleRefundExpired(ctx context.Context, refundExpiredEvent subpkg.RefundTransactionExpiredEvent) error {
	refunded, err := bridge.subClient.IsRefundedAlready(bridge.subClient.Identity, refundExpiredEvent.Hash)
	if err != nil {
		return err
	}

	if refunded {
		log.Info().Msgf("tx with stellar tx hash: %s is refunded already, skipping...", refundExpiredEvent.Hash)
		return pkg.ErrTransactionAlreadyRefunded
	}

	signature, sequenceNumber, err := bridge.wallet.CreateRefundAndReturnSignature(ctx, refundExpiredEvent.Target, refundExpiredEvent.Amount, refundExpiredEvent.Hash)
	if err != nil {
		return err
	}

	call, err := bridge.subClient.CreateRefundTransactionOrAddSig(bridge.subClient.Identity, refundExpiredEvent.Hash, refundExpiredEvent.Target, int64(refundExpiredEvent.Amount), signature, bridge.wallet.GetKeypair().Address(), sequenceNumber)
	if err != nil {
		return err
	}
	hash, err := bridge.subClient.CallExtrinsic(call)
	if err != nil {
		return err
	}
	log.Info().Msgf("call submitted with hash %s", hash.Hex())
	return nil
}

func (bridge *Bridge) handleRefundReady(ctx context.Context, refundReadyEvent subpkg.RefundTransactionReadyEvent) error {
	refunded, err := bridge.subClient.IsRefundedAlready(bridge.subClient.Identity, refundReadyEvent.Hash)
	if err != nil {
		return err
	}

	if refunded {
		log.Info().Msgf("tx with stellar tx hash: %s is refunded already, skipping...", refundReadyEvent.Hash)
		return pkg.ErrTransactionAlreadyRefunded
	}

	refund, err := bridge.subClient.GetRefundTransaction(bridge.subClient.Identity, refundReadyEvent.Hash)
	if err != nil {
		return err
	}

	err = bridge.wallet.CreateRefundPaymentWithSignaturesAndSubmit(ctx, refund.Target, uint64(refund.Amount), refund.TxHash, refund.Signatures, int64(refund.SequenceNumber))
	if err != nil {
		return err
	}

	call, err := bridge.subClient.SetRefundTransactionExecuted(bridge.subClient.Identity, refund.TxHash)
	if err != nil {
		return err
	}
	hash, err := bridge.subClient.CallExtrinsic(call)
	if err != nil {
		return err
	}
	log.Info().Msgf("call submitted with hash %s", hash.Hex())
	return nil
}

func (bridge *Bridge) handleWithdrawCreated(ctx context.Context, withdraw subpkg.WithdrawCreatedEvent) error {
	burned, err := bridge.subClient.IsBurnedAlready(bridge.subClient.Identity, types.U64(withdraw.ID))
	if err != nil {
		return err
	}

	if burned {
		log.Info().Msgf("tx with id: %d is burned already, skipping...", withdraw.ID)
		return errors.New("tx burned already")
	}

	if err := bridge.wallet.CheckAccount(withdraw.Target); err != nil {
		log.Info().Msgf("tx with id: %d is an invalid burn transaction, minting on chain again...", withdraw.ID)
		mintID := fmt.Sprintf("refund-%d", withdraw.ID)
		err := bridge.handleMint(big.NewInt(int64(withdraw.Amount)), substrate.AccountID(withdraw.Source), mintID)
		if err != nil {
			return err
		}
		log.Info().Msgf("setting invalid burn transaction (%d) as executed", withdraw.ID)
		call, err := bridge.subClient.SetBurnTransactionExecuted(bridge.subClient.Identity, withdraw.ID)
		if err != nil {
			return err
		}
		hash, err := bridge.subClient.CallExtrinsic(call)
		if err != nil {
			return err
		}
		log.Info().Msgf("call submitted with hash %s", hash)
		return nil
	}

	amount := big.NewInt(int64(withdraw.Amount))
	signature, sequenceNumber, err := bridge.wallet.CreatePaymentAndReturnSignature(ctx, withdraw.Target, amount.Uint64(), withdraw.ID)
	if err != nil {
		return err
	}
	log.Info().Msgf("stellar account sequence number: %d", sequenceNumber)

	call, err := bridge.subClient.ProposeBurnTransactionOrAddSig(bridge.subClient.Identity, withdraw.ID, withdraw.Target, amount, signature, bridge.wallet.GetKeypair().Address(), sequenceNumber)
	if err != nil {
		return err
	}
	hash, err := bridge.subClient.CallExtrinsic(call)
	if err != nil {
		return err
	}
	log.Info().Msgf("call submitted with hash %s", hash.Hex())

	return nil
}

func (bridge *Bridge) handleWithdrawExpired(ctx context.Context, withdrawExpired subpkg.WithdrawExpiredEvent) error {
	if err := bridge.wallet.CheckAccount(withdrawExpired.Target); err != nil {
		log.Info().Msgf("tx with id: %d is an invalid burn transaction, setting burn as executed since we have no way to recover...", withdrawExpired.ID)
		call, err := bridge.subClient.SetBurnTransactionExecuted(bridge.subClient.Identity, withdrawExpired.ID)
		if err != nil {
			return err
		}
		hash, err := bridge.subClient.CallExtrinsic(call)
		if err != nil {
			return err
		}
		log.Info().Msgf("call submitted with hash %s", hash)
		return nil
	}

	amount := big.NewInt(int64(withdrawExpired.Amount))
	signature, sequenceNumber, err := bridge.wallet.CreatePaymentAndReturnSignature(ctx, withdrawExpired.Target, amount.Uint64(), withdrawExpired.ID)
	if err != nil {
		return err
	}
	log.Info().Msgf("stellar account sequence number: %d", sequenceNumber)

	call, err := bridge.subClient.ProposeBurnTransactionOrAddSig(bridge.subClient.Identity, withdrawExpired.ID, withdrawExpired.Target, amount, signature, bridge.wallet.GetKeypair().Address(), sequenceNumber)
	if err != nil {
		return err
	}
	hash, err := bridge.subClient.CallExtrinsic(call)
	if err != nil {
		return err
	}
	log.Info().Msgf("call submitted with hash %s", hash.Hex())
	return nil
}

func (bridge *Bridge) handleWithdrawReady(ctx context.Context, withdrawReady subpkg.WithdrawReadyEvent) error {
	burned, err := bridge.subClient.IsBurnedAlready(bridge.subClient.Identity, types.U64(withdrawReady.ID))
	if err != nil {
		return err
	}

	if burned {
		log.Info().Msgf("tx with id: %d is burned already, skipping...", withdrawReady.ID)
		return errors.New("tx burned already")
	}

	burnTx, err := bridge.subClient.GetBurnTransaction(bridge.subClient.Identity, types.U64(withdrawReady.ID))
	if err != nil {
		return err
	}

	if len(burnTx.Signatures) == 0 {
		log.Info().Msg("found 0 signatures, aborting")
		return errors.New("no signatures")
	}

	// todo add memo hash
	err = bridge.wallet.CreatePaymentWithSignaturesAndSubmit(ctx, burnTx.Target, uint64(burnTx.Amount), "", burnTx.Signatures, int64(burnTx.SequenceNumber))
	if err != nil {
		return err
	}

	call, err := bridge.subClient.SetBurnTransactionExecuted(bridge.subClient.Identity, withdrawReady.ID)
	if err != nil {
		return err
	}
	hash, err := bridge.subClient.CallExtrinsic(call)
	if err != nil {
		return err
	}
	log.Info().Msgf("call submitted with hash %s", hash.Hex())
	return nil
}

func (bridge *Bridge) handleMint(amount *big.Int, target substrate.AccountID, mintID string) error {
	// TODO check if we already minted for this txid
	minted, err := bridge.subClient.IsMintedAlready(bridge.subClient.Identity, mintID)
	if err != nil && err != substrate.ErrMintTransactionNotFound {
		return err
	}

	if minted {
		log.Debug().Msgf("transaction with id %s is already minted", mintID)
		return errors.New("transaction already minted")
	}

	call, err := bridge.subClient.ProposeOrVoteMintTransaction(bridge.subClient.Identity, mintID, target, amount)
	if err != nil {
		return err
	}

	hash, err := bridge.subClient.CallExtrinsic(call)
	if err != nil {
		return err
	}
	log.Info().Msgf("mint call submitted with hash: %s", hash.Hex())
	return nil
}

func (bridge *Bridge) getSubstrateAddressFromMemo(memo string) (string, error) {
	chunks := strings.Split(memo, "_")
	if len(chunks) != 2 {
		// memo is not formatted correctly, issue a refund
		return "", errors.New("memo text is not correctly formatted")
	}

	id, err := strconv.Atoi(chunks[1])
	if err != nil {
		return "", err
	}

	switch chunks[0] {
	case "twin":
		twin, err := bridge.subClient.GetTwin(uint32(id))
		if err != nil {
			return "", err
		}
		return twin.Account.String(), nil
	case "farm":
		farm, err := bridge.subClient.GetFarm(uint32(id))
		if err != nil {
			return "", err
		}
		twin, err := bridge.subClient.GetTwin(uint32(farm.TwinID))
		if err != nil {
			return "", err
		}
		return twin.Account.String(), nil
	case "node":
		node, err := bridge.subClient.GetNode(uint32(id))
		if err != nil {
			return "", err
		}
		twin, err := bridge.subClient.GetTwin(uint32(node.TwinID))
		if err != nil {
			return "", err
		}
		return twin.Account.String(), nil
	case "entity":
		entity, err := bridge.subClient.GetEntity(uint32(id))
		if err != nil {
			return "", err
		}
		return entity.Account.String(), nil
	default:
		return "", errors.New("grid type not supported")
	}
}

func (bridge *Bridge) Close() error {
	bridge.mut.Lock()
	defer bridge.mut.Unlock()
	return nil
}
