package chain

import (
	"api/src/config"
	"api/src/utils"
	"context"
	"crypto/ecdsa"
	"fmt"
	"log"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/pkg/errors"
	"github.com/shopspring/decimal"
)

const DefaultTxTimeout = time.Second * 15

type Chain struct {
	InteractionAddress common.Address
	FeederAddress      common.Address
	USDTAddress        common.Address
	Client             *ethclient.Client
	Interaction        *Interaction
	Feeder             *Feeder
	Fees               *Fees
	USDT               *Token
	Name               string
	SwapperAddress     common.Address
}

type Interactor struct {
	PrivateKey  *ecdsa.PrivateKey
	UserAddress common.Address

	Chains map[int]*Chain

	GasMultiplier float64
}

func NewInteractor(config *config.Config) *Interactor {
	privateKey, err := crypto.HexToECDSA(config.PrivateKey)
	if err != nil {
		log.Fatalf("PK fail: %v", err)
	}

	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		log.Fatal("cannot assert type: publicKey is not of type *ecdsa.PublicKey")
	}

	chains := make(map[int]*Chain)
	for _, chainConfig := range config.Blockchains {
		client, err := ethclient.Dial(chainConfig.NodeAddress)
		if err != nil {
			log.Fatalf("Node dial failed: %v", err)
		}
		chain := Chain{
			InteractionAddress: common.HexToAddress(chainConfig.InteractionAddress),
			USDTAddress:        common.HexToAddress(chainConfig.UsdcAddress),
			SwapperAddress:     common.HexToAddress(chainConfig.SwapperAddress),
			Client:             client,
			FeederAddress:      common.Address{},
			Interaction:        nil,
			Feeder:             nil,
			USDT:               nil,
			Name:               chainConfig.Name,
		}
		interaction, err := NewInteraction(chain.InteractionAddress, chain.Client)
		if err != nil {
			log.Fatalf("Failed to connect with interaction contract: %v\n", err)
		}
		chain.Interaction = interaction

		feederAddress, err := chain.FundFactory.Feeder(nil)
		if err != nil {
			log.Fatalf("Failed to get feeder addresst: %v\n", err)
		}
		chain.FeederAddress = feederAddress
		feeder, err := NewFeeder(feederAddress, chain.Client)
		if err != nil {
			log.Fatalf("Failed to connect with feeder contract: %v\n", err)
		}
		chain.Feeder = feeder
		Usdt, err := NewToken(chain.USDTAddress, chain.Client)
		if err != nil {
			log.Fatalf("Failed to attach USDT contract: %s\n", err)
		}
		chain.USDT = Usdt

		log.Printf("Chain inited with\n"+
			"Contract address: %s\nFund factory address: %s\n"+
			"USDT address: %s\n",
			chain.InteractionAddress.Hex(),
			chain.USDTAddress.Hex(),
		)
		chains[chainConfig.ChainId] = &chain
	}

	interactor := Interactor{
		PrivateKey:    privateKey,
		UserAddress:   crypto.PubkeyToAddress(*publicKeyECDSA),
		Chains:        chains,
		GasMultiplier: 1.1,
	}
	log.Printf("Interaction inited with user address: %s\n",
		interactor.UserAddress,
	)
	return &interactor
}

func (interactor *Interactor) GetChain(chainId int) *Chain {
	return interactor.Chains[chainId]
}

func (interactor *Interactor) getAuth(chainId int) (*bind.TransactOpts, error) {

	nonce, err := interactor.Chains[chainId].Client.PendingNonceAt(context.Background(), interactor.UserAddress)
	if err != nil {
		return nil, err
	}

	chainIdBig := big.NewInt(int64(chainId))
	auth, err := bind.NewKeyedTransactorWithChainID(interactor.PrivateKey, chainIdBig)
	if err != nil {
		return nil, err
	}
	auth.Nonce = big.NewInt(int64(nonce))
	auth.Value = big.NewInt(0)      // in wei
	gasPrice, err := interactor.Chains[chainId].Client.SuggestGasPrice(context.Background())
	if err != nil {
		return nil, err
	}
	auth.GasPrice = gasPrice

	return auth, nil
}

// Returns a channel that blocks until the transaction is confirmed
func waitTxConfirmed(c *ethclient.Client, hash common.Hash) error {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTxTimeout)
	defer cancel()
	queryTicker := time.NewTicker(time.Second)
	defer queryTicker.Stop()
	for {
		_, err := c.TransactionReceipt(ctx, hash)
		if err == nil {
			fmt.Printf("Tx: %s mined\n", hash.String())
			return nil
		}

		if errors.Is(err, ethereum.NotFound) {
			fmt.Print("Transaction not yet mined\n")
		} else {
			fmt.Printf("Receipt retrieval failed: %s\n", err.Error())
		}

		// Wait for the next round.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-queryTicker.C:
		}
	}
}

func (interactor *Interactor) WithdrawMultiple(fundId *big.Int, tradeTvl *big.Int, chainId int) error {
	users, err := interactor.Chains[chainId].Feeder.UserWaitingForWithdrawal(nil, fundId)
	if err != nil {
		return err
	}
	opts, err := interactor.getAuth(chainId)
	if err != nil {
		return err
	}
	tx, err := interactor.Chains[chainId].Interaction.WithdrawMultiple(opts, fundId, users, tradeTvl)
	if err != nil {
		return err
	}
	err = waitTxConfirmed(interactor.Chains[chainId].Client, tx.Hash())
	if err != nil {
		return err
	}
	return nil
}

func (interactor *Interactor) UserData(chainId int, fundId *big.Int, user common.Address) (
	totalDeposit *big.Int, totalWithdrawals *big.Int, tokenAmount *big.Int, pendingWithdrawalTokens *big.Int, err error) {
	chain := interactor.Chains[chainId]
	if chain == nil {
		return nil, nil, nil, nil, fmt.Errorf("chain not found")
	}
	totalDeposit, totalWithdrawals, tokenAmount, pendingWithdrawalTokens, err = interactor.Chains[chainId].Feeder.GetUserData(nil, fundId, user)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return totalDeposit, totalWithdrawals, tokenAmount, pendingWithdrawalTokens, err
}

// Pack multiple swaps in one array
func (interactor *Interactor) MultiSwap(
	tradingAddress string,
	swapAddresses []string,
	tokensA []string,
	tokensB []string,
	amountsA []*big.Int,
	payloads []string,
	chainId int,
) error {
	opts, err := interactor.getAuth(chainId)
	if err != nil {
		return err
	}
	tradeContract, err := NewTrade(common.HexToAddress(tradingAddress), interactor.Chains[chainId].Client)
	if err != nil {
		return fmt.Errorf("failed to connect with trading contract: %v\n", err)
	}
	multiSwapData := make([][]byte, len(payloads))
	for i, _ := range payloads {
		addressType, _ := abi.NewType("address", "", nil)
		uintType, _ := abi.NewType("uint256", "", nil)
		bytesType, _ := abi.NewType("bytes", "", nil)
		args := abi.Arguments{
			{Type: addressType},
			{Type: addressType},
			{Type: addressType},
			{Type: uintType},
			{Type: bytesType},
		}
		data, err := args.Pack(
			common.HexToAddress(swapAddresses[i]),
			common.HexToAddress(tokensA[i]),
			common.HexToAddress(tokensB[i]),
			amountsA[i],
			hexutil.MustDecode(payloads[i]),
		)
		if err != nil {
			return fmt.Errorf("failed to create data: %v\n", err)
		}
		multiSwapData[i] = data
	}
	opts.GasLimit = uint64(float64(opts.GasLimit) * 1.2)
	tx, err := tradeContract.MultiSwap(
		opts,
		multiSwapData,
	)
	if err != nil {
		return err
	}
	err = waitTxConfirmed(interactor.Chains[chainId].Client, tx.Hash())
	if err != nil {
		return err
	}
	return nil
}

func (interactor *Interactor) AAVEPositions(
	tokens []string,
	tradingAddress common.Address,
	chainId int,
) ([]decimal.Decimal, error) {
	fmt.Printf("tradng address: %s\n", tradingAddress.Hex())
	tradeContract, err := NewTrade(tradingAddress, interactor.Chains[chainId].Client)
	if err != nil {
		return nil, fmt.Errorf("failed to connect with trading contract: %v\n", err)
	}
	assets := make([]common.Address, len(tokens))
	for i, token := range tokens {
		if token == "" {
			assets[i] = interactor.Chains[chainId].USDTAddress
		} else {
			assets[i] = common.HexToAddress(token)
		}
	}
	values, err := tradeContract.GetAavePositionSizes(nil, assets)
	if err != nil {
		return nil, fmt.Errorf("failed to get aave positions: %v", err)
	}
	positions := make([]decimal.Decimal, len(values))
	for i, value := range values {
		positions[i] = utils.WeiToDecimal(value)
	}
	return positions, nil
}

func (interactor *Interactor) AAVEWithdraw(
	token common.Address,
	amount decimal.Decimal,
	tradingAddress common.Address,
	chainId int,
) error {
	tradeContract, err := NewTrade(tradingAddress, interactor.Chains[chainId].Client)
	if err != nil {
		return fmt.Errorf("failed to connect with trading contract: %v\n", err)
	}
	opts, err := interactor.getAuth(chainId)
	if err != nil {
		return fmt.Errorf("failed to get opts for aave withdraw: %v", err)
	}
	tx, err := tradeContract.AaveWithdraw(opts, token, amount.BigInt())
	if err != nil {
		return fmt.Errorf("failed to execute aave withdraw: %v", err)
	}
	err = waitTxConfirmed(interactor.Chains[chainId].Client, tx.Hash())
	if err != nil {
		return err
	}
	return nil
}

func (interactor *Interactor) GmxPositions(
	collateralTokens []common.Address,
	indexTokens []common.Address,
	isLong []bool,
	tradeAddress common.Address,
	vaultAddress common.Address,
	readerAddress common.Address,
	chainId int,
) ([]*big.Int, error) {
	gmxReader, err := NewGmxReader(readerAddress, interactor.Chains[chainId].Client)
	if err != nil {
		return nil, fmt.Errorf("failed to connect with gmxReader contract: %v\n", err)
	}
	positionData, err := gmxReader.GetPositions(
		nil,
		vaultAddress,
		tradeAddress,
		collateralTokens,
		indexTokens,
		isLong,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch gmx positions: %v", err)
	}
	if err != nil {
		return nil, err
	}
	return positionData, nil
}

// Return raw multicall tx to be signed with frontend
func (Interactor *Interactor) GetMulticallTx(
	token string,
	amount *decimal.Decimal,
	targets []string,
	txs []string,
) (*string, error) {
	abi, err := ArbitrageMetaData.GetAbi()
	if err != nil {
		return nil, fmt.Errorf("failed to get arbitrage abi: %v", err)
	}
	addresses := make([]common.Address, len(targets))
	for i, t := range targets {
		addresses[i] = common.HexToAddress(t)
	}
	data := make([][]byte, len(txs))
	for i, p := range txs {
		data[i], err = hexutil.Decode(p)
		if err != nil {
			return nil, fmt.Errorf("failed to decode tx: %v", err)
		}
	}
	packedTx, err := abi.Pack("multiSwap", common.HexToAddress(token), amount.BigInt(), addresses, data)
	if err != nil {
		return nil, fmt.Errorf("failed to pack multicall tx: %v", err)
	}
	encoded := fmt.Sprintf("0x%s", hex.EncodeToString(packedTx))
	return &encoded, nil
}

