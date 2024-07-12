package main

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"

	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/mr-tron/base58"
	pb "github.com/rpcpool/yellowstone-grpc/examples/golang/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

type ParsedTransaction struct {
	DiffMints           []string
	DiffOwners          []string
	DiffAmounts         []int64
	AccountKeysExtended []string
	Signer              string
	Fee                 uint64
	Signature           string
	Instructions        []ParsedInstruction
}

type ParsedInstruction struct {
	OutAmount          uint64
	OutAccounts        []string
	InAmount           uint64
	InAccounts         []string
	PrecedingProgramId string
	PrecedingAccounts  []string
}

var (
	grpcAddr           = flag.String("endpoint", "", "Solana gRPC address, in URI format e.g. https://api.rpcpool.com")
	token              = flag.String("x-token", "", "Token for authenticating")
	jsonInput          = flag.String("json", "", "JSON for subscription request, prefix with @ to read json from file")
	insecureConnection = flag.Bool("insecure", false, "Connect without TLS")
	slots              = flag.Bool("slots", false, "Subscribe to slots update")
	blocks             = flag.Bool("blocks", false, "Subscribe to block update")
	block_meta         = flag.Bool("blocks-meta", false, "Subscribe to block metadata update")
	signature          = flag.String("signature", "", "Subscribe to a specific transaction signature")
	resub              = flag.Uint("resub", 0, "Resubscribe to only slots after x updates, 0 disables this")

	accounts = flag.Bool("accounts", false, "Subscribe to accounts")

	transactions       = flag.Bool("transactions", false, "Subscribe to transactions, required for tx_account_include/tx_account_exclude and vote/failed.")
	voteTransactions   = flag.Bool("transactions-vote", false, "Include vote transactions")
	failedTransactions = flag.Bool("transactions-failed", false, "Include failed transactions")

	accountsFilter              arrayFlags
	accountOwnersFilter         arrayFlags
	transactionsAccountsInclude arrayFlags
	transactionsAccountsExclude arrayFlags
)

var kacp = keepalive.ClientParameters{
	Time:                10 * time.Second, // send pings every 10 seconds if there is no activity
	Timeout:             time.Second,      // wait 1 second for ping ack before considering the connection dead
	PermitWithoutStream: true,             // send pings even without active streams
}

func base58ToHex(s string) string {
	decoded, err := base58.Decode(s)
	if err != nil {
		return s
	}
	return hex.EncodeToString(decoded)
}

// Helper function to convert protobuf to JSON string
func protoToJSON(msg proto.Message) string {
	marshaler := jsonpb.Marshaler{
		EmitDefaults: true,
		OrigName:     true,
	}
	jsonStr, err := marshaler.MarshalToString(msg)
	if err != nil {
		log.Printf("Error marshaling to JSON: %v", err)
		return ""
	}
	return jsonStr
}

func processValue(v interface{}) interface{} {
	switch val := v.(type) {
	case string:
		// Try to parse as integer first
		if intVal, err := strconv.ParseInt(val, 10, 64); err == nil {
			return intVal
		}
		// If not an integer, decode base64 and encode to base58
		decoded, err := base64.StdEncoding.DecodeString(val)
		if err == nil {
			return base58.Encode(decoded)
		}
		if len(val) > 0 && (val[0] <= 32 || val[0] >= 127) {
			return base58.Encode([]byte(val))
		}
		return val
	case float64:
		if float64(int64(val)) == val {
			return int64(val)
		}
	case []interface{}:
		for i, item := range val {
			val[i] = processValue(item)
		}
	case map[string]interface{}:
		for k, item := range val {
			if item == nil {
				continue
			}
			switch k {
			case "accounts", "writable_indexes", "readonly_indexes":
				if accounts, ok := item.(string); ok {
					decoded, err := base64.StdEncoding.DecodeString(accounts)
					if err == nil {
						accountInts := make([]int, len(decoded))
						for i, b := range decoded {
							accountInts[i] = int(b)
						}
						val[k] = accountInts
					}
				}
			case "instructions", "innerInstructions":
				if instructions, ok := item.([]interface{}); ok {
					for i, inst := range instructions {
						if instMap, ok := inst.(map[string]interface{}); ok {
							if accounts, ok := instMap["accounts"].(string); ok {
								decoded, err := base64.StdEncoding.DecodeString(accounts)
								if err == nil {
									accountInts := make([]int, len(decoded))
									for i, b := range decoded {
										accountInts[i] = int(b)
									}
									instMap["accounts"] = accountInts
								}
							}
							if data, ok := instMap["data"].(string); ok {
								decoded, _ := base64.StdEncoding.DecodeString(data)
								instMap["data"] = hex.EncodeToString(decoded)
							}
						}
						instructions[i] = inst
					}
					val[k] = instructions
				}
			case "mint", "owner", "program_id":
				// Do nothing, leave these values as they are
			default:
				val[k] = processValue(item)
			}
		}
	}
	return v
}

// Helper function to process the JSON and encode binary data to base58
func processJSON(jsonStr string) string {
	var jsonData interface{}
	err := json.Unmarshal([]byte(jsonStr), &jsonData)
	if err != nil {
		log.Printf("Error unmarshaling JSON: %v", err)
		return jsonStr
	}

	processedData := processValue(jsonData)
	processedJSON, err := json.Marshal(processedData)
	if err != nil {
		log.Printf("Error marshaling processed JSON: %v", err)
		return jsonStr
	}

	return string(processedJSON)
}

func parseTransactionNew(resp *pb.SubscribeUpdate) map[string]interface{} {
	result := make(map[string]interface{})

	transaction := resp.GetTransaction()
	if transaction == nil || transaction.Transaction == nil {
		return result
	}

	txInfo := transaction.Transaction

	// Add slot information
	result["slot"] = transaction.Slot

	// Step 1: Parse token balances
	preBalances := txInfo.Meta.PreTokenBalances
	postBalances := txInfo.Meta.PostTokenBalances
	for i, post := range postBalances {
		result[fmt.Sprintf("diff_mint_%d", i)] = post.Mint
		result[fmt.Sprintf("diff_owner_%d", i)] = post.Owner
		postAmount, _ := strconv.ParseInt(post.UiTokenAmount.Amount, 10, 64)

		var preAmount int64
		if i < len(preBalances) {
			preAmount, _ = strconv.ParseInt(preBalances[i].UiTokenAmount.Amount, 10, 64)
		}

		result[fmt.Sprintf("diff_amount_%d", i)] = postAmount - preAmount
	}

	// Step 2: Parse account keys
	accountKeys := txInfo.Transaction.Message.AccountKeys
	extendedKeys := append(append(accountKeys, txInfo.Meta.LoadedWritableAddresses...), txInfo.Meta.LoadedReadonlyAddresses...)
	for i, key := range extendedKeys {
		result[fmt.Sprintf("accounts_keys_extended_%d", i)] = base58.Encode(key)
	}

	// Step 3: Get transaction signer
	if len(accountKeys) > 0 {
		result["signer"] = base58.Encode(accountKeys[0])
	}

	// Step 4: Get transaction fee
	result["fee"] = txInfo.Meta.Fee

	// Step 5: Get signature
	if len(txInfo.Transaction.Signatures) > 0 {
		result["signature"] = base58.Encode(txInfo.Transaction.Signatures[0])
	}

	// Step 6 & 7: Parse instructions
	jupiterProgramId := "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"

	for i, inst := range txInfo.Transaction.Message.Instructions {
		programId := base58.Encode(extendedKeys[inst.ProgramIdIndex])

		if programId == jupiterProgramId {
			// Parse inner instructions
			for _, innerInst := range txInfo.Meta.InnerInstructions {
				if innerInst.Index == uint32(i) {
					parseSwapInstructions(innerInst.Instructions, extendedKeys, result)
				}
			}
			break
		}
	}

	return result
}

func parseSwapInstructions(instructions []*pb.InnerInstruction, accountKeys [][]byte, result map[string]interface{}) {
    tokenProgramId := "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
    var pairIndex int
    var precedingInstruction *pb.InnerInstruction
    
    for i := 0; i < len(instructions); i++ {
        instruction := instructions[i]
        programId := base58.Encode(accountKeys[instruction.ProgramIdIndex])
        
        if programId == tokenProgramId {
            if i > 0 && base58.Encode(accountKeys[instructions[i-1].ProgramIdIndex]) != tokenProgramId {
                precedingInstruction = instructions[i-1]
            } else {
                continue
            }
            
            // Parse the pair of token program instructions
            outInstruction := instruction
            if i+1 < len(instructions) {
                inInstruction := instructions[i+1]
                
                // Extract amounts
                outAmount := parseInstructionAmount(outInstruction.Data)
                inAmount := parseInstructionAmount(inInstruction.Data)
                
                result[fmt.Sprintf("instruction_out_amount_%d", pairIndex)] = outAmount
                result[fmt.Sprintf("instruction_in_amount_%d", pairIndex)] = inAmount
                
                // Extract accounts
                for j, acc := range outInstruction.Accounts {
                    result[fmt.Sprintf("instruction_out_accounts_%d_%d", pairIndex, j)] = base58.Encode(accountKeys[acc])
                }
                for j, acc := range inInstruction.Accounts {
                    result[fmt.Sprintf("instruction_in_accounts_%d_%d", pairIndex, j)] = base58.Encode(accountKeys[acc])
                }
                
                result[fmt.Sprintf("instruction_preceeding_program_id_%d", pairIndex)] = base58.Encode(accountKeys[precedingInstruction.ProgramIdIndex])
                for j, acc := range precedingInstruction.Accounts {
                    result[fmt.Sprintf("instruction_preceeding_accounts_%d_%d", pairIndex, j)] = base58.Encode(accountKeys[acc])
                }
                
                pairIndex++
                i++ // Skip the next instruction as we've already processed it
            }
        }
    }

func parseInstructionAmount(data []byte) interface{} {
	// Output data as hex
	// log.Println("Data: ", hex.EncodeToString(data))
	if len(data) < 9 {
		return 0
	}
	value := binary.LittleEndian.Uint64(data[1:9])
	// log.Println("Value: ", value)

	return value
}

// Helper function to reverse byte order (convert from little-endian to big-endian)
func reverseBytes(b []byte) []byte {
	reversed := make([]byte, len(b))
	for i := 0; i < len(b); i++ {
		reversed[i] = b[len(b)-1-i]
	}
	return reversed
}

func main() {
	log.SetFlags(0)

	flag.Var(&accountsFilter, "accounts-account", "Subscribe to an account, may be specified multiple times to subscribe to multiple accounts.")
	flag.Var(&accountOwnersFilter, "accounts-owner", "Subscribe to an account owner, may be specified multiple times to subscribe to multiple account owners.")
	flag.Var(&transactionsAccountsInclude, "transactions-account-include", "Subscribe to transactions mentioning an account, may be specified multiple times to subscribe to multiple accounts.")
	flag.Var(&transactionsAccountsExclude, "transactions-account-exclude", "Subscribe to transactions not mentioning an account, may be specified multiple times to exclude multiple accounts.")

	flag.Parse()

	if *grpcAddr == "" {
		log.Fatalf("GRPC address is required. Please provide --endpoint parameter.")
	}

	u, err := url.Parse(*grpcAddr)
	if err != nil {
		log.Fatalf("Invalid GRPC address provided: %v", err)
	}

	// Infer insecure connection if http is given
	if u.Scheme == "http" {
		*insecureConnection = true
	}

	port := u.Port()
	if port == "" {
		if *insecureConnection {
			port = "80"
		} else {
			port = "443"
		}
	}
	hostname := u.Hostname()
	if hostname == "" {
		log.Fatalf("Please provide URL format endpoint e.g. http(s)://<endpoint>:<port>")
	}

	address := hostname + ":" + port

	conn := grpc_connect(address, *insecureConnection)
	defer conn.Close()

	grpc_subscribe(conn)
}

func grpc_connect(address string, plaintext bool) *grpc.ClientConn {
	var opts []grpc.DialOption
	if plaintext {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		pool, _ := x509.SystemCertPool()
		creds := credentials.NewClientTLSFromCert(pool, "")
		opts = append(opts, grpc.WithTransportCredentials(creds))
	}

	opts = append(opts, grpc.WithKeepaliveParams(kacp))

	log.Println("Starting grpc client, connecting to", address)
	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		log.Fatalf("fail to dial: %v", err)
	}

	return conn
}

func grpc_subscribe(conn *grpc.ClientConn) {
	var err error
	client := pb.NewGeyserClient(conn)

	var subscription pb.SubscribeRequest

	// Read json input or JSON file prefixed with @
	if *jsonInput != "" {
		var jsonData []byte

		if (*jsonInput)[0] == '@' {
			jsonData, err = os.ReadFile((*jsonInput)[1:])
			if err != nil {
				log.Fatalf("Error reading provided json file: %v", err)
			}
		} else {
			jsonData = []byte(*jsonInput)
		}
		err := json.Unmarshal(jsonData, &subscription)
		if err != nil {
			log.Fatalf("Error parsing JSON: %v", err)
		}
	} else {
		// If no JSON provided, start with blank
		subscription = pb.SubscribeRequest{}
	}

	// We append to the JSON provided maps. If JSON provides a map item
	// with the exact same ID, then this will override that sub.
	if *slots {
		if subscription.Slots == nil {
			subscription.Slots = make(map[string]*pb.SubscribeRequestFilterSlots)
		}

		subscription.Slots["slots"] = &pb.SubscribeRequestFilterSlots{}

	}

	if *blocks {
		if subscription.Blocks == nil {
			subscription.Blocks = make(map[string]*pb.SubscribeRequestFilterBlocks)
		}
		subscription.Blocks["blocks"] = &pb.SubscribeRequestFilterBlocks{}
	}

	if *block_meta {
		if subscription.BlocksMeta == nil {
			subscription.BlocksMeta = make(map[string]*pb.SubscribeRequestFilterBlocksMeta)
		}
		subscription.BlocksMeta["block_meta"] = &pb.SubscribeRequestFilterBlocksMeta{}
	}

	if (len(accountsFilter)+len(accountOwnersFilter)) > 0 || (*accounts) {
		if subscription.Accounts == nil {
			subscription.Accounts = make(map[string]*pb.SubscribeRequestFilterAccounts)
		}

		subscription.Accounts["account_sub"] = &pb.SubscribeRequestFilterAccounts{}

		if len(accountsFilter) > 0 {
			subscription.Accounts["account_sub"].Account = accountsFilter
		}

		if len(accountOwnersFilter) > 0 {
			subscription.Accounts["account_sub"].Owner = accountOwnersFilter
		}
	}

	// Set up the transactions subscription
	if subscription.Transactions == nil {
		subscription.Transactions = make(map[string]*pb.SubscribeRequestFilterTransactions)
	}

	// Subscribe to a specific signature
	if *signature != "" {
		tr := true
		subscription.Transactions["signature_sub"] = &pb.SubscribeRequestFilterTransactions{
			Failed: &tr,
			Vote:   &tr,
		}

		if *signature != "" {
			subscription.Transactions["signature_sub"].Signature = signature
		}
	}

	// Subscribe to generic transaction stream
	if *transactions {

		subscription.Transactions["transactions_sub"] = &pb.SubscribeRequestFilterTransactions{
			Failed: failedTransactions,
			Vote:   voteTransactions,
		}

		subscription.Transactions["transactions_sub"].AccountInclude = transactionsAccountsInclude
		subscription.Transactions["transactions_sub"].AccountExclude = transactionsAccountsExclude
	}

	subscriptionJson, err := json.Marshal(&subscription)
	if err != nil {
		log.Printf("Failed to marshal subscription request: %v", subscriptionJson)
	}
	// log.Printf("Subscription request: %s", string(subscriptionJson))

	// Set up the subscription request
	ctx := context.Background()
	if *token != "" {
		md := metadata.New(map[string]string{"x-token": *token})
		ctx = metadata.NewOutgoingContext(ctx, md)
	}

	stream, err := client.Subscribe(ctx)
	if err != nil {
		log.Fatalf("%v", err)
	}
	err = stream.Send(&subscription)
	if err != nil {
		log.Fatalf("%v", err)
	}

	var i uint = 0
	log.Printf("%v\t%v\t%v\t%v", "timestamp", "signature", "transaction_parsed_main", "transaction_json")

	for i < 1000 {
		i += 1
		if i == *resub {
			subscription = pb.SubscribeRequest{}
			subscription.Slots = make(map[string]*pb.SubscribeRequestFilterSlots)
			subscription.Slots["slots"] = &pb.SubscribeRequestFilterSlots{}
			stream.Send(&subscription)
		}

		resp, err := stream.Recv()
		timestamp := time.Now().UnixNano()
		signature := resp.GetTransaction().GetTransaction().GetSignature()
		base58Signature := base58.Encode(signature)
		parsedTransactionNew := parseTransactionNew(resp)
		parsedTransactionJSON, err := json.Marshal(parsedTransactionNew)

		if err == io.EOF {
			return
		}
		if err != nil {
			log.Fatalf("Error occurred in receiving update: %v", err)
		}
		jsonStr := protoToJSON(resp)
		processedJSON := processJSON(jsonStr)
		log.Printf("%v\t%v\t%v\t%v", timestamp, base58Signature, string(parsedTransactionJSON), processedJSON)
	}
}
