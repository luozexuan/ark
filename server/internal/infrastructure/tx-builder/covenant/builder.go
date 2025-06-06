package txbuilder

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math"

	"github.com/ark-network/ark/common"
	"github.com/ark-network/ark/common/tree"
	"github.com/ark-network/ark/server/internal/core/domain"
	"github.com/ark-network/ark/server/internal/core/ports"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/vulpemventures/go-elements/address"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/network"
	"github.com/vulpemventures/go-elements/psetv2"
	"github.com/vulpemventures/go-elements/taproot"
	"github.com/vulpemventures/go-elements/transaction"
)

type txBuilder struct {
	wallet            ports.WalletService
	net               common.Network
	vtxoTreeExpiry    common.RelativeLocktime
	boardingExitDelay common.RelativeLocktime
}

func NewTxBuilder(
	wallet ports.WalletService,
	net common.Network,
	vtxoTreeExpiry common.RelativeLocktime,
	boardingExitDelay common.RelativeLocktime,
) ports.TxBuilder {
	return &txBuilder{wallet, net, vtxoTreeExpiry, boardingExitDelay}
}

func (b *txBuilder) GetTxID(tx string) (string, error) {
	return getTxid(tx)
}

func (b *txBuilder) BuildSweepTx(inputs []ports.SweepInput) (txid, signedSweepTx string, err error) {
	sweepPset, err := sweepTransaction(
		b.wallet,
		inputs,
		b.onchainNetwork().AssetID,
	)
	if err != nil {
		return "", "", err
	}

	sweepPsetBase64, err := sweepPset.ToBase64()
	if err != nil {
		return "", "", err
	}

	ctx := context.Background()
	signedSweepPsetB64, err := b.wallet.SignTransactionTapscript(ctx, sweepPsetBase64, nil)
	if err != nil {
		return "", "", err
	}

	signedPset, err := psetv2.NewPsetFromBase64(signedSweepPsetB64)
	if err != nil {
		return "", "", err
	}

	if err := psetv2.FinalizeAll(signedPset); err != nil {
		return "", "", err
	}

	extractedTx, err := psetv2.Extract(signedPset)
	if err != nil {
		return "", "", err
	}

	txhex, err := extractedTx.ToHex()
	if err != nil {
		return "", "", err
	}

	return extractedTx.TxHash().String(), txhex, nil
}

func (b *txBuilder) VerifyForfeitTxs(
	vtxos []domain.Vtxo, connectors tree.TxTree,
	forfeitTxs []string, connectorIndex map[string]domain.Outpoint,
) (map[domain.VtxoKey]string, error) {
	connectorsLeaves := connectors.Leaves()

	forfeitAddress, err := b.wallet.GetForfeitAddress(context.Background())
	if err != nil {
		return nil, err
	}

	forfeitScript, err := address.ToOutputScript(forfeitAddress)
	if err != nil {
		return nil, err
	}

	minRate := b.wallet.MinRelayFeeRate(context.Background())

	validForfeitTxs := make(map[domain.VtxoKey]string)

	blocktimestamp, err := b.wallet.GetCurrentBlockTime(context.Background())
	if err != nil {
		return nil, err
	}

	dustAmount, err := b.wallet.GetDustAmount(context.Background())
	if err != nil {
		return nil, err
	}

	for _, b64 := range forfeitTxs {
		tx, err := psetv2.NewPsetFromBase64(b64)
		if err != nil {
			return nil, err
		}

		connectorInput := tx.Inputs[0]
		vtxoInput := tx.Inputs[1]
		vtxoKey := domain.VtxoKey{
			Txid: chainhash.Hash(vtxoInput.PreviousTxid).String(),
			VOut: vtxoInput.PreviousTxIndex,
		}

		// check if the connector outpoint is the one associated with the vtxo
		expectedConnectorOutpoint, ok := connectorIndex[vtxoKey.String()]
		if !ok {
			return nil, fmt.Errorf("invalid connector outpoint for vtxo %s", vtxoKey)
		}

		if chainhash.Hash(connectorInput.PreviousTxid).String() != expectedConnectorOutpoint.Txid ||
			connectorInput.PreviousTxIndex != expectedConnectorOutpoint.VOut {
			return nil, fmt.Errorf(
				"invalid connector outpoint for vtxo %s, wrong outpoint, expected %s",
				vtxoKey,
				domain.VtxoKey(expectedConnectorOutpoint),
			)
		}

		if _, hasValidForfeit := validForfeitTxs[vtxoKey]; hasValidForfeit {
			continue
		}

		var vtxo *domain.Vtxo
		for _, v := range vtxos {
			if v.VtxoKey == vtxoKey {
				vtxo = &v
				break
			}
		}

		if vtxo == nil {
			return nil, fmt.Errorf("missing vtxo %s", vtxoKey)
		}

		feeAmount := uint64(0)
		for _, output := range tx.Outputs {
			if len(output.Script) <= 0 {
				feeAmount = output.Value
				break
			}
		}

		if feeAmount == 0 {
			return nil, fmt.Errorf("missing forfeit tx fee output")
		}

		var connectorOutput *psetv2.Output
		for _, leaf := range connectorsLeaves {
			if leaf.Txid == chainhash.Hash(connectorInput.PreviousTxid).String() {
				connectorTx, err := psetv2.NewPsetFromBase64(leaf.Tx)
				if err != nil {
					return nil, err
				}

				if len(connectorTx.Outputs) <= int(connectorInput.PreviousTxIndex) {
					return nil, fmt.Errorf("invalid connector tx")
				}

				connectorOutput = &connectorTx.Outputs[connectorInput.PreviousTxIndex]
				break
			}
		}

		if connectorOutput == nil {
			return nil, fmt.Errorf("missing connector tx")
		}

		inputAmount := vtxo.Amount + uint64(connectorOutput.Value)

		if feeAmount > inputAmount {
			return nil, fmt.Errorf("forfeit tx fee is higher than the input amount, %d > %d", feeAmount, inputAmount)
		}

		if len(tx.Inputs[1].TapLeafScript) <= 0 {
			return nil, fmt.Errorf("missing taproot leaf script for vtxo input, invalid forfeit tx")
		}

		vtxoTapscript := tx.Inputs[1].TapLeafScript[0]
		conditionWitness, err := tree.GetConditionWitness(tx.Inputs[1])
		if err != nil {
			return nil, err
		}
		conditionWitnessSize := 0
		for _, witness := range conditionWitness {
			conditionWitnessSize += len(witness)
		}

		// verify the forfeit closure script
		closure, err := tree.DecodeClosure(vtxoTapscript.Script)
		if err != nil {
			return nil, err
		}

		var locktime *common.AbsoluteLocktime

		switch c := closure.(type) {
		case *tree.CLTVMultisigClosure:
			locktime = &c.Locktime
		case *tree.MultisigClosure, *tree.ConditionMultisigClosure:
		default:
			return nil, fmt.Errorf("invalid forfeit closure script %x, cannot verify forfeit tx", vtxoTapscript.Script)
		}

		if locktime != nil {
			if !locktime.IsSeconds() {
				if *locktime > common.AbsoluteLocktime(blocktimestamp.Time) {
					return nil, fmt.Errorf("forfeit closure is CLTV locked, %d > %d (block time)", *locktime, blocktimestamp.Time)
				}
			} else {
				if *locktime > common.AbsoluteLocktime(blocktimestamp.Height) {
					return nil, fmt.Errorf("forfeit closure is CLTV locked, %d > %d (block height)", *locktime, blocktimestamp.Height)
				}
			}
		}

		minFee, err := common.ComputeForfeitTxFee(
			minRate,
			&waddrmgr.Tapscript{
				RevealedScript: vtxoTapscript.Script,
				ControlBlock:   &vtxoTapscript.ControlBlock.ControlBlock,
			},
			closure.WitnessSize(conditionWitnessSize),
			txscript.GetScriptClass(forfeitScript),
		)
		if err != nil {
			return nil, err
		}

		if inputAmount-feeAmount < dustAmount {
			return nil, fmt.Errorf("forfeit tx output amount is dust, %d < %d", inputAmount-feeAmount, dustAmount)
		}

		if feeAmount < uint64(minFee) {
			return nil, fmt.Errorf("forfeit tx fee is lower than the min relay fee, %d < %d", feeAmount, minFee)
		}

		feeThreshold := uint64(math.Ceil(float64(minFee) * 1.05))

		if feeAmount > feeThreshold {
			return nil, fmt.Errorf("forfeit tx fee is higher than 5%% of the min relay fee, %d > %d", feeAmount, feeThreshold)
		}

		vtxoInputArgs := psetv2.InputArgs{
			Txid:    chainhash.Hash(vtxoInput.PreviousTxid).String(),
			TxIndex: vtxoInput.PreviousTxIndex,
		}

		if locktime != nil {
			vtxoInputArgs.TimeLock = uint32(*locktime)
			vtxoInputArgs.Sequence = wire.MaxTxInSequenceNum - 1
		}

		vtxoTapKey, err := vtxo.TapKey()
		if err != nil {
			return nil, err
		}

		vtxoScript, err := common.P2TRScript(vtxoTapKey)
		if err != nil {
			return nil, err
		}

		rebuilt, err := tree.BuildForfeitTx(
			b.onchainNetwork().AssetID,
			psetv2.InputArgs{
				Txid:    chainhash.Hash(connectorInput.PreviousTxid).String(),
				TxIndex: connectorInput.PreviousTxIndex,
			},
			vtxoInputArgs,
			vtxo.Amount,
			connectorOutput.Value,
			feeAmount,
			vtxoScript,
			connectorOutput.Script,
			forfeitScript,
		)
		if err != nil {
			return nil, err
		}

		rebuiltUtx, err := rebuilt.UnsignedTx()
		if err != nil {
			return nil, err
		}

		forfeitUtx, err := tx.UnsignedTx()
		if err != nil {
			return nil, err
		}

		if rebuiltUtx.TxHash().String() != forfeitUtx.TxHash().String() {
			return nil, fmt.Errorf("invalid forfeit tx")
		}

		validForfeitTxs[vtxoKey] = b64
	}

	return validForfeitTxs, nil
}

func (b *txBuilder) BuildRoundTx(
	serverPubkey *secp256k1.PublicKey,
	requests []domain.TxRequest,
	boardingInputs []ports.BoardingInput,
	connectorAddresses []string,
	_ []*tree.Musig2,
) (roundTx string, vtxoTree tree.TxTree, nextConnectorAddress string, connectors tree.TxTree, err error) {
	// The creation of the tree and the round tx are tightly coupled:
	// - building the tree requires knowing the shared outpoint (txid:vout)
	// - building the round tx requires knowing the shared output script and amount
	// The idea here is to first create all the data for the outputs of the txs
	// of the vtxo tree to calculate the shared output script and amount.
	// With these data the round tx can be created, and once the shared utxo
	// outpoint is obtained, the vtxo tree can be finally created.
	// The factory function `treeFactoryFn` returned below holds all outputs data
	// generated in the process and takes the shared utxo outpoint as argument.
	// This is safe as the memory allocated for `BuildVtxoTree` is flushed
	// only after `BuildRoundTx` returns.

	var (
		sharedOutputScript []byte
		sharedOutputAmount uint64
		vtxoTreeFactory    tree.TreeFactory
	)

	if !isOnchainOnly(requests) {
		feeSatsPerNode, err := b.wallet.MinRelayFee(context.Background(), uint64(common.CovenantTreeTxSize))
		if err != nil {
			return "", nil, "", nil, err
		}

		vtxosLeaves, err := getOutputVtxosLeaves(requests)
		if err != nil {
			return "", nil, "", nil, err
		}

		vtxoTreeFactory, sharedOutputScript, sharedOutputAmount, err = tree.BuildTxTree(
			b.onchainNetwork().AssetID, serverPubkey, vtxosLeaves, feeSatsPerNode, &b.vtxoTreeExpiry,
		)
		if err != nil {
			return "", nil, "", nil, err
		}
	}

	var (
		connectorsSharedOutputAmount uint64
		connectorsSharedOutputScript []byte
		connectorsTreeFactory        tree.TreeFactory
	)

	numberOfConnectors := countSpentVtxos(requests)

	if numberOfConnectors > 0 {
		var err error
		connectorAmount, err := b.wallet.GetDustAmount(context.Background())
		if err != nil {
			return "", nil, "", nil, err
		}

		nextConnectorAddress, err = b.wallet.DeriveConnectorAddress(context.Background())
		if err != nil {
			return "", nil, "", nil, err
		}

		connectorPkScript, err := address.ToOutputScript(nextConnectorAddress)
		if err != nil {
			return "", nil, "", nil, err
		}

		connectorsLeaves := make([]tree.Leaf, 0)
		for i := uint64(0); i < numberOfConnectors; i++ {
			connectorsLeaves = append(connectorsLeaves, tree.Leaf{
				Amount: connectorAmount,
				Script: hex.EncodeToString(connectorPkScript),
			})
		}

		connectorNodeFeeAmount, err := b.minRelayFeeConnectorTx()
		if err != nil {
			return "", nil, "", nil, err
		}

		connectorsTreeFactory, connectorsSharedOutputScript, connectorsSharedOutputAmount, err = tree.BuildTxTree(
			b.onchainNetwork().AssetID,
			nil,
			connectorsLeaves,
			connectorNodeFeeAmount,
			nil,
		)
		if err != nil {
			return "", nil, "", nil, err
		}
	}

	ptx, err := b.createRoundTx(
		sharedOutputAmount, sharedOutputScript,
		requests, boardingInputs,
		serverPubkey,
		connectorsSharedOutputAmount, connectorsSharedOutputScript,
		connectorAddresses,
	)
	if err != nil {
		return
	}

	unsignedTx, err := ptx.UnsignedTx()
	if err != nil {
		return
	}

	if vtxoTreeFactory != nil {
		vtxoTree, err = vtxoTreeFactory(psetv2.InputArgs{
			Txid:    unsignedTx.TxHash().String(),
			TxIndex: 0,
		})
		if err != nil {
			return
		}
	}

	if connectorsTreeFactory != nil {
		connectors, err = connectorsTreeFactory(psetv2.InputArgs{
			Txid:    unsignedTx.TxHash().String(),
			TxIndex: 1,
		})
		if err != nil {
			return
		}
	}

	roundTx, err = ptx.ToBase64()
	if err != nil {
		return
	}

	return
}

func (b *txBuilder) GetSweepInput(node tree.Node) (vtxoTreeExpiry *common.RelativeLocktime, sweepInput ports.SweepInput, err error) {
	pset, err := psetv2.NewPsetFromBase64(node.Tx)
	if err != nil {
		return nil, nil, err
	}

	if len(pset.Inputs) != 1 {
		return nil, nil, fmt.Errorf("invalid node pset, expect 1 input, got %d", len(pset.Inputs))
	}

	// if the tx is not onchain, it means that the input is an existing shared output
	input := pset.Inputs[0]
	txid := chainhash.Hash(input.PreviousTxid).String()
	index := input.PreviousTxIndex

	sweepLeaf, vtxoTreeExpiry, err := extractSweepLeaf(input)
	if err != nil {
		return nil, nil, err
	}

	txhex, err := b.wallet.GetTransaction(context.Background(), txid)
	if err != nil {
		return nil, nil, err
	}

	tx, err := transaction.NewTxFromHex(txhex)
	if err != nil {
		return nil, nil, err
	}

	inputValue, err := elementsutil.ValueFromBytes(tx.Outputs[index].Value)
	if err != nil {
		return nil, nil, err
	}

	sweepInput = &sweepLiquidInput{
		inputArgs: psetv2.InputArgs{
			Txid:    txid,
			TxIndex: index,
		},
		sweepLeaf: sweepLeaf,
		amount:    inputValue,
	}

	return vtxoTreeExpiry, sweepInput, nil
}
func (b *txBuilder) VerifyTapscriptPartialSigs(tx string) (bool, string, error) {
	pset, err := psetv2.NewPsetFromBase64(tx)
	if err != nil {
		return false, "", err
	}

	return b.verifyTapscriptPartialSigs(pset)
}

func (b *txBuilder) verifyTapscriptPartialSigs(pset *psetv2.Pset) (bool, string, error) {
	utx, _ := pset.UnsignedTx()
	txid := utx.TxHash().String()

	serverPubkey, err := b.wallet.GetPubkey(context.Background())
	if err != nil {
		return false, "", err
	}

	for index, input := range pset.Inputs {
		if len(input.TapLeafScript) == 0 {
			continue
		}
		if input.WitnessUtxo == nil {
			return false, txid, fmt.Errorf("missing prevout for input %d", index)
		}

		// verify taproot leaf script
		tapLeaf := input.TapLeafScript[0]

		closure, err := tree.DecodeClosure(tapLeaf.Script)
		if err != nil {
			return false, txid, err
		}

		keys := make(map[string]bool)

		switch c := closure.(type) {
		case *tree.MultisigClosure:
			for _, key := range c.PubKeys {
				keys[hex.EncodeToString(schnorr.SerializePubKey(key))] = false
			}
		case *tree.CSVMultisigClosure:
			for _, key := range c.PubKeys {
				keys[hex.EncodeToString(schnorr.SerializePubKey(key))] = false
			}
		case *tree.CLTVMultisigClosure:
			for _, key := range c.PubKeys {
				keys[hex.EncodeToString(schnorr.SerializePubKey(key))] = false
			}
		case *tree.ConditionMultisigClosure:
			witness, err := tree.GetConditionWitness(input)
			if err != nil {
				return false, txid, err
			}

			result, err := tree.ExecuteBoolScript(c.Condition, witness)
			if err != nil {
				return false, txid, err
			}

			if !result {
				return false, txid, fmt.Errorf("condition not met for input %d", index)
			}

			for _, key := range c.PubKeys {
				keys[hex.EncodeToString(schnorr.SerializePubKey(key))] = false
			}
		}

		// we don't need to check if server signed
		keys[hex.EncodeToString(schnorr.SerializePubKey(serverPubkey))] = true

		rootHash := tapLeaf.ControlBlock.RootHash(tapLeaf.Script)
		tapKeyFromControlBlock := taproot.ComputeTaprootOutputKey(tree.UnspendableKey(), rootHash[:])

		pkscript, err := common.P2TRScript(tapKeyFromControlBlock)
		if err != nil {
			return false, txid, err
		}

		if !bytes.Equal(pkscript, input.WitnessUtxo.Script) {
			return false, txid, fmt.Errorf("invalid control block for input %d", index)
		}

		leafHash := taproot.NewBaseTapElementsLeaf(tapLeaf.Script).TapHash()

		preimage, err := b.getTaprootPreimage(
			pset,
			index,
			&leafHash,
		)
		if err != nil {
			return false, txid, err
		}

		for _, tapScriptSig := range input.TapScriptSig {
			sig, err := schnorr.ParseSignature(tapScriptSig.Signature)
			if err != nil {
				return false, txid, err
			}

			pubkey, err := schnorr.ParsePubKey(tapScriptSig.PubKey)
			if err != nil {
				return false, txid, err
			}

			if !sig.Verify(preimage, pubkey) {
				return false, txid, nil
			}

			keys[hex.EncodeToString(schnorr.SerializePubKey(pubkey))] = true
		}

		missingSigs := 0
		for key := range keys {
			if !keys[key] {
				missingSigs++
			}
		}

		if missingSigs > 0 {
			return false, txid, fmt.Errorf("missing %d signatures", missingSigs)
		}
	}

	return true, txid, nil
}

func (b *txBuilder) FinalizeAndExtract(tx string) (string, error) {
	ptx, err := psetv2.NewPsetFromBase64(tx)
	if err != nil {
		return "", err
	}

	for i, in := range ptx.Inputs {
		if in.WitnessUtxo == nil {
			return "", fmt.Errorf("missing witness utxo, cannot finalize tx")
		}

		if len(in.TapLeafScript) > 0 {
			tapLeaf := in.TapLeafScript[0]

			closure, err := tree.DecodeClosure(tapLeaf.Script)
			if err != nil {
				return "", err
			}

			conditionWitness, err := tree.GetConditionWitness(in)
			if err != nil {
				return "", err
			}

			args := make(map[string][]byte)
			if len(conditionWitness) > 0 {
				var conditionWitnessBytes bytes.Buffer
				if err := psbt.WriteTxWitness(&conditionWitnessBytes, conditionWitness); err != nil {
					return "", err
				}
				args[tree.ConditionWitnessKey] = conditionWitnessBytes.Bytes()
			}

			for _, sig := range in.TapScriptSig {
				args[hex.EncodeToString(sig.PubKey)] = sig.Signature
			}

			controlBlock, err := tapLeaf.ControlBlock.ToBytes()
			if err != nil {
				return "", err
			}

			witness, err := closure.Witness(controlBlock, args)
			if err != nil {
				return "", err
			}

			var witnessBuf bytes.Buffer
			if err := psbt.WriteTxWitness(&witnessBuf, witness); err != nil {
				return "", err
			}

			ptx.Inputs[i].FinalScriptWitness = witnessBuf.Bytes()
			continue
		}

		if err := psetv2.Finalize(ptx, i); err != nil {
			return "", fmt.Errorf("failed to finalize signed pset: %s", err)
		}
	}

	// extract the forfeit tx
	extracted, err := psetv2.Extract(ptx)
	if err != nil {
		return "", err
	}

	return extracted.ToHex()
}

func (b *txBuilder) FindLeaves(
	vtxoTree tree.TxTree,
	fromtxid string,
	fromvout uint32,
) ([]tree.Node, error) {
	allLeaves := vtxoTree.Leaves()
	foundLeaves := make([]tree.Node, 0)

	for _, leaf := range allLeaves {
		branch, err := vtxoTree.Branch(leaf.Txid)
		if err != nil {
			return nil, err
		}

		for _, node := range branch {
			ptx, err := psetv2.NewPsetFromBase64(node.Tx)
			if err != nil {
				return nil, err
			}

			if len(ptx.Inputs) <= 0 {
				return nil, fmt.Errorf("no input in the pset")
			}

			parentInput := ptx.Inputs[0]

			hash, err := chainhash.NewHash(parentInput.PreviousTxid)
			if err != nil {
				return nil, err
			}

			if hash.String() == fromtxid && parentInput.PreviousTxIndex == fromvout {
				foundLeaves = append(foundLeaves, leaf)
				break
			}
		}
	}

	return foundLeaves, nil
}

func (b *txBuilder) createRoundTx(
	sharedOutputAmount uint64,
	sharedOutputScript []byte,
	requests []domain.TxRequest,
	boardingInputs []ports.BoardingInput,
	serverPubkey *secp256k1.PublicKey,
	connectorSharedOutputAmount uint64,
	connectorSharedOutputScript []byte,
	connectorAddresses []string,
) (*psetv2.Pset, error) {
	serverScript, err := p2wpkhScript(serverPubkey, b.onchainNetwork())
	if err != nil {
		return nil, err
	}

	targetAmount := uint64(0)

	outputs := make([]psetv2.OutputArgs, 0)

	if sharedOutputScript != nil && sharedOutputAmount > 0 {
		targetAmount += sharedOutputAmount

		outputs = append(outputs, psetv2.OutputArgs{
			Asset:  b.onchainNetwork().AssetID,
			Amount: sharedOutputAmount,
			Script: sharedOutputScript,
		})
	}

	if connectorSharedOutputScript != nil && connectorSharedOutputAmount > 0 {
		targetAmount += connectorSharedOutputAmount

		outputs = append(outputs, psetv2.OutputArgs{
			Asset:  b.onchainNetwork().AssetID,
			Amount: connectorSharedOutputAmount,
			Script: connectorSharedOutputScript,
		})
	}

	onchainOutputs, err := getOnchainOutputs(requests, b.onchainNetwork())
	if err != nil {
		return nil, err
	}

	for _, out := range onchainOutputs {
		targetAmount += out.Amount
	}

	outputs = append(outputs, onchainOutputs...)

	for _, in := range boardingInputs {
		targetAmount -= in.Amount
	}
	ctx := context.Background()

	dustLimit, err := b.wallet.GetDustAmount(ctx)
	if err != nil {
		return nil, err
	}

	utxos, change, err := b.selectUtxos(ctx, connectorAddresses, targetAmount)
	if err != nil {
		return nil, err
	}

	var dust uint64
	if change > 0 {
		if change < dustLimit {
			dust = change
			change = 0
		} else {
			outputs = append(outputs, psetv2.OutputArgs{
				Asset:  b.onchainNetwork().AssetID,
				Amount: change,
				Script: serverScript,
			})
		}
	}

	ptx, err := psetv2.New(nil, outputs, nil)
	if err != nil {
		return nil, err
	}

	updater, err := psetv2.NewUpdater(ptx)
	if err != nil {
		return nil, err
	}

	for _, in := range boardingInputs {
		if err := updater.AddInputs(
			[]psetv2.InputArgs{
				{
					Txid:    in.Txid,
					TxIndex: in.VtxoKey.VOut,
				},
			},
		); err != nil {
			return nil, err
		}

		index := len(ptx.Inputs) - 1

		assetBytes, err := elementsutil.AssetHashToBytes(b.onchainNetwork().AssetID)
		if err != nil {
			return nil, fmt.Errorf("failed to convert asset to bytes: %s", err)
		}

		valueBytes, err := elementsutil.ValueToBytes(in.Amount)
		if err != nil {
			return nil, fmt.Errorf("failed to convert value to bytes: %s", err)
		}

		boardingVtxoScript, err := tree.ParseVtxoScript(in.Tapscripts)
		if err != nil {
			return nil, err
		}

		boardingTapKey, _, err := boardingVtxoScript.TapTree()
		if err != nil {
			return nil, err
		}

		boardingOutputScript, err := common.P2TRScript(boardingTapKey)
		if err != nil {
			return nil, err
		}

		if err := updater.AddInWitnessUtxo(index, transaction.NewTxOutput(assetBytes, valueBytes, boardingOutputScript)); err != nil {
			return nil, err
		}

		if err := updater.AddInSighashType(index, txscript.SigHashDefault); err != nil {
			return nil, err
		}
	}

	if err := addInputs(updater, utxos); err != nil {
		return nil, err
	}

	b64, err := ptx.ToBase64()
	if err != nil {
		return nil, err
	}

	feeAmount, err := b.wallet.EstimateFees(ctx, b64)
	if err != nil {
		return nil, err
	}

	if dust > feeAmount {
		feeAmount = dust
	} else {
		feeAmount += dust
	}

	if dust == 0 {
		if feeAmount == change {
			// fees = change, remove change output
			ptx.Outputs = ptx.Outputs[:len(ptx.Outputs)-1]
			ptx.Global.OutputCount--
			feeAmount += change
		} else if feeAmount < change {
			// change covers the fees, reduce change amount
			if change-feeAmount < dustLimit {
				ptx.Outputs = ptx.Outputs[:len(ptx.Outputs)-1]
				ptx.Global.OutputCount--
				feeAmount += change
			} else {
				ptx.Outputs[len(ptx.Outputs)-1].Value = change - feeAmount
			}
		} else {
			// change is not enough to cover fees, re-select utxos
			if change > 0 {
				// remove change output if present
				ptx.Outputs = ptx.Outputs[:len(ptx.Outputs)-1]
				ptx.Global.OutputCount--
			}
			newUtxos, change, err := b.selectUtxos(ctx, connectorAddresses, feeAmount-change)
			if err != nil {
				return nil, err
			}

			if change > 0 {
				if change < dustLimit {
					feeAmount += change
				} else {
					if err := updater.AddOutputs([]psetv2.OutputArgs{
						{
							Asset:  b.onchainNetwork().AssetID,
							Amount: change,
							Script: serverScript,
						},
					}); err != nil {
						return nil, err
					}
				}
			}

			if err := addInputs(updater, newUtxos); err != nil {
				return nil, err
			}
		}
	} else if feeAmount-dust > 0 {
		newUtxos, change, err := b.selectUtxos(ctx, connectorAddresses, feeAmount-dust)
		if err != nil {
			return nil, err
		}

		if change > 0 {
			if change < dustLimit {
				feeAmount += change
			} else {
				if err := updater.AddOutputs([]psetv2.OutputArgs{
					{
						Asset:  b.onchainNetwork().AssetID,
						Amount: change,
						Script: serverScript,
					},
				}); err != nil {
					return nil, err
				}
			}
		}

		if err := addInputs(updater, newUtxos); err != nil {
			return nil, err
		}
	}

	// add fee output
	if err := updater.AddOutputs([]psetv2.OutputArgs{
		{
			Asset:  b.onchainNetwork().AssetID,
			Amount: feeAmount,
		},
	}); err != nil {
		return nil, err
	}

	return ptx, nil
}

func (b *txBuilder) minRelayFeeConnectorTx() (uint64, error) {
	return b.wallet.MinRelayFee(context.Background(), uint64(common.ConnectorTxSize))
}

func (b *txBuilder) CountSignedTaprootInputs(tx string) (int, error) {
	ptx, err := psetv2.NewPsetFromBase64(tx)
	if err != nil {
		return -1, err
	}

	signedInputsCount := 0
	for _, input := range ptx.Inputs {
		if len(input.TapScriptSig) == 0 || len(input.TapLeafScript) == 0 {
			continue
		}
		signedInputsCount++
	}
	return signedInputsCount, nil
}

// This method aims to verify and add partial signature from boarding input
func (b *txBuilder) VerifyAndCombinePartialTx(dest string, src string) (string, error) {
	roundPset, err := psetv2.NewPsetFromBase64(dest)
	if err != nil {
		return "", err
	}

	sourcePset, err := psetv2.NewPsetFromBase64(src)
	if err != nil {
		return "", err
	}

	roundUtx, err := roundPset.UnsignedTx()
	if err != nil {
		return "", err
	}

	sourceUtx, err := sourcePset.UnsignedTx()
	if err != nil {
		return "", err
	}

	if roundUtx.TxHash().String() != sourceUtx.TxHash().String() {
		return "", fmt.Errorf("txid mismatch")
	}

	roundSigner, err := psetv2.NewSigner(roundPset)
	if err != nil {
		return "", err
	}

	for i, input := range sourcePset.Inputs {
		if len(input.TapScriptSig) == 0 || len(input.TapLeafScript) == 0 {
			continue
		}

		partialSig := input.TapScriptSig[0]

		leafHash, err := chainhash.NewHash(partialSig.LeafHash)
		if err != nil {
			return "", err
		}

		preimage, err := b.getTaprootPreimage(sourcePset, i, leafHash)
		if err != nil {
			return "", err
		}

		sig, err := schnorr.ParseSignature(partialSig.Signature)
		if err != nil {
			return "", err
		}

		pubkey, err := schnorr.ParsePubKey(partialSig.PubKey)
		if err != nil {
			return "", err
		}

		if !sig.Verify(preimage, pubkey) {
			return "", fmt.Errorf("invalid signature")
		}

		if err := roundSigner.AddInTapLeafScript(i, input.TapLeafScript[0]); err != nil {
			return "", err
		}

		if err := roundSigner.SignTaprootInputTapscriptSig(i, partialSig); err != nil {
			return "", err
		}
	}

	return roundSigner.Pset.ToBase64()
}

func (b *txBuilder) getTaprootPreimage(pset *psetv2.Pset, inputIndex int, leafHash *chainhash.Hash) ([]byte, error) {
	prevoutScripts := make([][]byte, 0)
	prevoutAssets := make([][]byte, 0)
	prevoutValues := make([][]byte, 0)

	for i, input := range pset.Inputs {
		if input.WitnessUtxo == nil {
			return nil, fmt.Errorf("missing witness utxo on input #%d", i)
		}

		prevoutScripts = append(prevoutScripts, input.WitnessUtxo.Script)
		prevoutAssets = append(prevoutAssets, input.WitnessUtxo.Asset)
		prevoutValues = append(prevoutValues, input.WitnessUtxo.Value)
	}

	utx, err := pset.UnsignedTx()
	if err != nil {
		return nil, err
	}

	genesisHash, _ := chainhash.NewHashFromStr(b.onchainNetwork().GenesisBlockHash)

	preimage := utx.HashForWitnessV1(
		inputIndex, prevoutScripts, prevoutAssets, prevoutValues,
		pset.Inputs[inputIndex].SigHashType, genesisHash, leafHash, nil,
	)
	return preimage[:], nil
}

func (b *txBuilder) onchainNetwork() *network.Network {
	switch b.net.Name {
	case common.Liquid.Name:
		return &network.Liquid
	case common.LiquidTestNet.Name:
		return &network.Testnet
	case common.LiquidRegTest.Name:
		return &network.Regtest
	default:
		return &network.Liquid
	}
}

func extractSweepLeaf(input psetv2.Input) (sweepLeaf *psetv2.TapLeafScript, vtxoTreeExpiry *common.RelativeLocktime, err error) {
	for _, leaf := range input.TapLeafScript {
		closure := &tree.CSVMultisigClosure{}
		valid, err := closure.Decode(leaf.Script)
		if err != nil {
			return nil, nil, err
		}
		if valid && (vtxoTreeExpiry == nil || vtxoTreeExpiry.LessThan(closure.Locktime)) {
			sweepLeaf = &leaf
			vtxoTreeExpiry = &closure.Locktime
		}
	}

	if sweepLeaf == nil {
		return nil, nil, fmt.Errorf("sweep leaf not found")
	}

	return
}

type sweepLiquidInput struct {
	inputArgs psetv2.InputArgs
	sweepLeaf *psetv2.TapLeafScript
	amount    uint64
}

func (s *sweepLiquidInput) GetAmount() uint64 {
	return s.amount
}

func (s *sweepLiquidInput) GetControlBlock() []byte {
	ctrlBlock, _ := s.sweepLeaf.ControlBlock.ToBytes()
	return ctrlBlock
}

func (s *sweepLiquidInput) GetHash() chainhash.Hash {
	h, _ := chainhash.NewHashFromStr(s.inputArgs.Txid)
	return *h
}

func (s *sweepLiquidInput) GetIndex() uint32 {
	return s.inputArgs.TxIndex
}

func (s *sweepLiquidInput) GetInternalKey() *secp256k1.PublicKey {
	return s.sweepLeaf.ControlBlock.InternalKey
}

func (s *sweepLiquidInput) GetLeafScript() []byte {
	return s.sweepLeaf.Script
}
