package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"
	"time"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/account"
	"github.com/NethermindEth/starknet.go/rpc"
	"github.com/NethermindEth/starknet.go/utils"
	"github.com/joho/godotenv"
)

const (
	DEFAULT_MAX_BATCH_SIZE = 2
	MAX_FEE                = 10000000000000 // 0.00001 ETH
	WAITING_TIME           = time.Duration(10 * time.Second)
)

var (
	colors = []string{
		"\033[31m",
		"\033[32m",
		"\033[33m",
		"\033[34m",
		"\033[35m",
		"\033[36m",
	} // ANSI color codes
	reset = "\033[0m"
)

type Batcher struct {
	accnt           *account.Account
	contractAddress *felt.Felt
	maxSize         int
	inChan          <-chan []string
	failChan        chan<- []string
}

func NewBatcher(
	accnt *account.Account,
	contractAddress *felt.Felt,
	maxSize int,
	inChan <-chan []string,
	failChan chan<- []string,
) *Batcher {
	return &Batcher{
		accnt:           accnt,
		contractAddress: contractAddress,
		maxSize:         maxSize,
		inChan:          inChan,
		failChan:        failChan,
	}
}

type TxnDataPair struct {
	Txn  rpc.BroadcastInvokev1Txn
	Data [][]string
}

func (b *Batcher) Run() {
	txnDataPairChan := make(chan TxnDataPair)

	go b.runBuildActor(txnDataPairChan)
	go b.runSendActor(txnDataPairChan)
}

//------------------------------------------------------------------------------
// Custom Printf functions
//------------------------------------------------------------------------------

func (b *Batcher) buildPrintf(index int, args ...interface{}) {
	color := colors[index]
	fmt.Printf("%sBatcher | build actor | ", color)
	fmt.Print(args...)
	fmt.Printf("%s\n", reset)
}

func (b *Batcher) sendPrintf(index int, args ...interface{}) {
	color := colors[index]
	fmt.Printf("%sBatcher |  send actor | ", color)
	fmt.Print(args...)
	fmt.Printf("%s\n", reset)
}

//------------------------------------------------------------------------------
// Builder methods
//------------------------------------------------------------------------------

func (b *Batcher) buildFunctionCall(data []string) (*rpc.FunctionCall, error) {
	toAddressInFelt, err := utils.HexToFelt(data[0])
	if err != nil {
		return nil, fmt.Errorf("error converting the to address to Felt: %v", err)
	}

	nftID, err := strconv.Atoi(data[1])
	if err != nil {
		return nil, fmt.Errorf("error converting the NFT ID to int: %v", err)
	}

	return &rpc.FunctionCall{
		ContractAddress: b.contractAddress,
		EntryPointSelector: utils.GetSelectorFromNameFelt(
			"safe_transfer_from",
		),
		Calldata: []*felt.Felt{
			b.accnt.AccountAddress,
			toAddressInFelt,
			new(felt.Felt).SetUint64(uint64(nftID)),
			new(felt.Felt).SetUint64(0), // data -> None
			new(felt.Felt).SetUint64(0), // extra data -> None
		},
	}, nil
}

func (b *Batcher) buildBatchTransaction(functionCalls []rpc.FunctionCall) (rpc.BroadcastInvokev1Txn, error) {
	calldata, err := b.accnt.FmtCalldata(functionCalls)
	if err != nil {
		return rpc.BroadcastInvokev1Txn{}, fmt.Errorf("error formatting the calldata: %v", err)
	}

	return rpc.BroadcastInvokev1Txn{
		InvokeTxnV1: rpc.InvokeTxnV1{
			MaxFee:        new(felt.Felt).SetUint64(MAX_FEE),
			Version:       rpc.TransactionV1,
			Nonce:         new(felt.Felt).SetUint64(0), // Will be set by the send actor
			Type:          rpc.TransactionType_Invoke,
			SenderAddress: b.accnt.AccountAddress,
			Calldata:      calldata,
		},
	}, nil
}

func (b *Batcher) runBuildActor(txnDataPairChan chan<- TxnDataPair) {
	fmt.Println("batcher | build actor | Starting actor")

	index := 0
	size := 0
	functionCalls := make([]rpc.FunctionCall, 0, b.maxSize)
	currentData := make([][]string, 0, b.maxSize)

mainLoop:
	for {
		trigger := false

		select {
		case data, ok := <-b.inChan:
			if !ok {
				break mainLoop
			}

			b.buildPrintf(index, "Received new data")

			functionCall, err := b.buildFunctionCall(data)
			if err != nil {
				b.buildPrintf(index, fmt.Sprintf("Error building function call: %v", err))
				b.failChan <- data
				continue
			}

			b.buildPrintf(index, "Function call built")

			functionCalls = append(functionCalls, *functionCall)
			size++
			currentData = append(currentData, data)

			if size >= b.maxSize {
				b.buildPrintf(index, "Batch size reached")
				trigger = true
			}

		case <-time.After(WAITING_TIME):
			if size > 0 {
				b.buildPrintf(index, "Timeout reached")
				trigger = true
			}
		}

		if trigger {
			builtTxn, err := b.buildBatchTransaction(functionCalls)
			if err != nil {
				b.buildPrintf(index, fmt.Sprintf("Error building batch transaction: %v", err))
				for _, data := range currentData {
					b.failChan <- data
				}
			} else {
				b.buildPrintf(index, "Batch transaction built")

				txnDataPairChan <- TxnDataPair{
					Txn:  builtTxn,
					Data: currentData,
				}

				b.buildPrintf(index, "Batch transaction sent to send actor")
			}

			index = (index + 1) % len(colors)
			size = 0
			functionCalls = make([]rpc.FunctionCall, 0, b.maxSize)
			currentData = make([][]string, 0, b.maxSize)
		}
	}

	fmt.Println("batcher | build actor | Exiting actor")
}

//------------------------------------------------------------------------------
// Send methods
//------------------------------------------------------------------------------

func (b *Batcher) runSendActor(txnDataPairChan <-chan TxnDataPair) {
	fmt.Println("batcher |  send actor | Starting actor")

	index := len(colors) - 1
	oldNonce := new(felt.Felt).SetUint64(0)

	for {
		index = (index + 1) % len(colors)

		txnDataPair, ok := <-txnDataPairChan
		if !ok {
			break
		}
		txn := txnDataPair.Txn
		data := txnDataPair.Data

		b.sendPrintf(index, "Received new batch transaction")

		nonce, err := b.accnt.Nonce(
			context.Background(),
			rpc.BlockID{Tag: "latest"},
			b.accnt.AccountAddress,
		)
		if err != nil {
			b.sendPrintf(index, fmt.Sprintf("Error getting the account nonce: %v", err))
			for _, data := range data {
				b.failChan <- data
			}
			continue
		}

		if nonce.Cmp(oldNonce) <= 0 {
			// The nonce has not been updated yet, update it manually
			nonce.Add(oldNonce, new(felt.Felt).SetUint64(1))
		}

		b.sendPrintf(index, fmt.Sprintf("Account nonce: %s", nonce.Text(10)))
		txn.InvokeTxnV1.Nonce = nonce

		err = b.accnt.SignInvokeTransaction(
			context.Background(),
			&txn.InvokeTxnV1,
		)
		if err != nil {
			b.sendPrintf(index, fmt.Sprintf("Error signing the transaction: %v", err))
			for _, data := range data {
				b.failChan <- data
			}
			continue
		}

		b.sendPrintf(index, "Transaction signed")

		// b.sendPrintf(index, "Estimating the transaction fee")
		// fee, err := b.accnt.EstimateFee(
		// 	context.Background(),
		// 	[]rpc.BroadcastTxn{txn},
		// 	[]rpc.SimulationFlag{},
		// 	rpc.WithBlockTag("latest"),
		// )
		// if err != nil {
		// 	b.sendPrintf(index, fmt.Sprintf("Error estimating the fee: %v", err))
		// 	for _, data := range data {
		// 		b.failChan <- data
		// 	}
		// 	continue
		// }

		// if fee[0].OverallFee.Cmp(new(felt.Felt).SetUint64(MAX_FEE)) > 0 {
		// 	b.sendPrintf(index, fmt.Sprintf("Estimated fee is too high: %s (max: %d)", fee[0].OverallFee.Text(10), MAX_FEE))
		// 	for _, data := range data {
		// 		b.failChan <- data
		// 	}
		// 	continue
		// }

		// b.sendPrintf(index, fmt.Sprintf("Estimated fee: %s", fee[0].OverallFee.Text(10)))

		resp, err := b.accnt.SendTransaction(
			context.Background(),
			&txn,
		)
		if err != nil {
			b.sendPrintf(index, fmt.Sprintf("Error adding the transaction: %v", err))
			for _, data := range data {
				b.failChan <- data
			}
			continue
		}

		b.sendPrintf(index, "Transaction submitted")
		b.sendPrintf(index, fmt.Sprintf("Transaction hash: %s", resp.TransactionHash))

	statusLoop:
		for {
			time.Sleep(time.Second * 5)

			txStatus, err := b.accnt.GetTransactionStatus(
				context.Background(),
				resp.TransactionHash,
			)
			if err != nil {
				b.sendPrintf(index, fmt.Sprintf("Error getting the transaction status: %v", err))
				for _, data := range data {
					b.failChan <- data
				}
				break
			}

			// NOTE: status can be empty
			b.sendPrintf(index, fmt.Sprintf("Transaction execution status: %s", txStatus.ExecutionStatus))
			b.sendPrintf(index, fmt.Sprintf("Transaction finality status: %s", txStatus.FinalityStatus))

			switch txStatus.ExecutionStatus {
			case rpc.TxnExecutionStatusSUCCEEDED:
				b.sendPrintf(index, "Transaction succeeded")
				oldNonce = nonce
				break statusLoop
			case rpc.TxnExecutionStatusREVERTED:
				b.sendPrintf(index, "Transaction reverted")
				oldNonce = nonce // A reverted transaction consumes the nonce
				for _, data := range data {
					b.failChan <- data
				}
				break statusLoop
			default:
			}

			switch txStatus.FinalityStatus {
			case rpc.TxnStatus_Received:
				continue
			case rpc.TxnStatus_Accepted_On_L2, rpc.TxnStatus_Accepted_On_L1:
				b.sendPrintf(index, "Transaction succeeded")
				oldNonce = nonce
				break statusLoop
			case rpc.TxnStatus_Rejected:
				b.sendPrintf(index, "Transaction rejected")
				for _, data := range data {
					b.failChan <- data
				}
				break statusLoop
			default:
			}
		}
	}

	fmt.Println("batcher |  send actor | Exiting actor")
}

//------------------------------------------------------------------------------
// Main
//------------------------------------------------------------------------------

func getData(filename string) ([][]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = 2
	reader.TrimLeadingSpace = true

	return reader.ReadAll()
}

func handleFailures(failChan <-chan []string) {
	failedTxnsFile, err := os.Create("failedTxns.csv")
	if err != nil {
		log.Fatal("Error creating failedTxns.csv: ", err)
	}
	defer failedTxnsFile.Close()

	writer := csv.NewWriter(failedTxnsFile)

	for {
		data, ok := <-failChan
		if !ok {
			break
		}

		err := writer.Write(data)
		if err != nil {
			fmt.Println("Error writing to failedTxns.csv: ", err)
		}
		writer.Flush()
	}
}

func main() {
	if len(os.Args) != 3 {
		log.Fatal("Usage: go run main.go <private_key> <csv_file>")
	}

	envFile, err := godotenv.Read()
	if err != nil {
		log.Fatal("Error loading .env file: ", err)
	}

	ks := account.NewMemKeystore()
	privKeyBI, ok := new(big.Int).SetString(os.Args[1], 0)
	if !ok {
		log.Fatal("Invalid private key")
	}
	ks.Put("", privKeyBI)

	data, err := getData(os.Args[2])
	if err != nil {
		log.Fatal("Error reading the CSV file: ", err)
	}

	client, err := rpc.NewProvider(envFile["RPC_URL"])
	if err != nil {
		log.Fatal("Error creating the RPC client: ", err)
	}

	accountAddressInFelt, err := utils.HexToFelt(envFile["ACCOUNT_ADDRESS"])
	if err != nil {
		log.Fatal("Error converting the account address to Felt: ", err)
	}

	accountCairoVersion, err := strconv.Atoi(envFile["ACCOUNT_CAIRO_VERSION"])
	if err != nil {
		log.Fatal("Error converting the account Cairo version to int: ", err)
	}

	accnt, err := account.NewAccount(
		client,
		accountAddressInFelt,
		"",
		ks,
		accountCairoVersion,
	)
	if err != nil {
		log.Fatal("Error creating the account: ", err)
	}

	contractAddressInFelt, err := utils.HexToFelt(
		envFile["NFT_CONTRACT_ADDRESS"],
	)
	if err != nil {
		log.Fatal("Error converting the contract address to Felt: ", err)
	}

	fmt.Printf("\tSender address: %s\n", accnt.AccountAddress)
	fmt.Printf("\tContract address: %s\n", contractAddressInFelt)
	fmt.Printf("\tNumber of transactions: %d\n", len(data))
	fmt.Println("Initialization completed successfully")

	inChan := make(chan []string, 1000)
	failChan := make(chan []string, 1000)

	batcher := NewBatcher(
		accnt,
		contractAddressInFelt,
		DEFAULT_MAX_BATCH_SIZE,
		inChan,
		failChan,
	)

	batcher.Run()
	go handleFailures(failChan)

	time.Sleep(time.Second * 2)

	for _, d := range data {
		inChan <- d
	}

	select {}
}
