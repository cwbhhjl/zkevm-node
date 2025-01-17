package aggregator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/aggregator/metrics"
	"github.com/0xPolygonHermez/zkevm-node/aggregator/pb"
	"github.com/0xPolygonHermez/zkevm-node/aggregator/prover"
	ethmanTypes "github.com/0xPolygonHermez/zkevm-node/etherman/types"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/state"
	"google.golang.org/grpc"
	grpchealth "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"
)

const (
	mockedStateRoot     = "0x090bcaf734c4f06c93954a827b45a6e8c67b8e0fd1e0a35a1c5982d6961828f9"
	mockedLocalExitRoot = "0x17c04c3760510b48c6012742c540a81aba4bca2f78b9d14bfd2f123e2e53ea3e"
)

type finalProofMsg struct {
	proverID       string
	recursiveProof *state.Proof
	finalProof     *pb.FinalProof
}

// Aggregator represents an aggregator
type Aggregator struct {
	pb.UnimplementedAggregatorServiceServer

	cfg Config

	State                   stateInterface
	EthTxManager            ethTxManager
	Ethman                  etherman
	ProfitabilityChecker    aggregatorTxProfitabilityChecker
	TimeSendFinalProof      time.Time
	StateDBMutex            *sync.Mutex
	TimeSendFinalProofMutex *sync.RWMutex

	finalProof     chan finalProofMsg
	verifyingProof bool

	srv  *grpc.Server
	ctx  context.Context
	exit context.CancelFunc
}

// New creates a new aggregator.
func New(
	cfg Config,
	stateInterface stateInterface,
	ethTxManager ethTxManager,
	etherman etherman,
) (Aggregator, error) {
	var profitabilityChecker aggregatorTxProfitabilityChecker
	switch cfg.TxProfitabilityCheckerType {
	case ProfitabilityBase:
		profitabilityChecker = NewTxProfitabilityCheckerBase(stateInterface, cfg.IntervalAfterWhichBatchConsolidateAnyway.Duration, cfg.TxProfitabilityMinReward.Int)
	case ProfitabilityAcceptAll:
		profitabilityChecker = NewTxProfitabilityCheckerAcceptAll(stateInterface, cfg.IntervalAfterWhichBatchConsolidateAnyway.Duration)
	}

	a := Aggregator{
		cfg: cfg,

		State:                   stateInterface,
		EthTxManager:            ethTxManager,
		Ethman:                  etherman,
		ProfitabilityChecker:    profitabilityChecker,
		StateDBMutex:            &sync.Mutex{},
		TimeSendFinalProofMutex: &sync.RWMutex{},

		finalProof: make(chan finalProofMsg),
	}

	return a, nil
}

// Start starts the aggregator
func (a *Aggregator) Start(ctx context.Context) error {
	var cancel context.CancelFunc
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel = context.WithCancel(ctx)
	a.ctx = ctx
	a.exit = cancel

	metrics.Register()

	// Delete ungenerated recursive proofs
	err := a.State.DeleteUngeneratedProofs(ctx, nil)
	if err != nil {
		return fmt.Errorf("Failed to initialize proofs cache %w", err)
	}

	address := fmt.Sprintf("%s:%d", a.cfg.Host, a.cfg.Port)
	lis, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	a.srv = grpc.NewServer()
	pb.RegisterAggregatorServiceServer(a.srv, a)

	healthService := newHealthChecker()
	grpchealth.RegisterHealthServer(a.srv, healthService)

	go func() {
		log.Infof("Server listening on port %d", a.cfg.Port)
		if err := a.srv.Serve(lis); err != nil {
			a.exit()
			log.Fatalf("Failed to serve: %v", err)
		}
	}()

	a.resetVerifyProofTime()

	go a.sendFinalProof()

	<-ctx.Done()
	return ctx.Err()
}

// Stop stops the Aggregator server.
func (a *Aggregator) Stop() {
	a.exit()
	a.srv.Stop()
}

// Channel implements the bi-directional communication channel between the
// Prover client and the Aggregator server.
func (a *Aggregator) Channel(stream pb.AggregatorService_ChannelServer) error {
	metrics.ConnectedProver()
	defer metrics.DisconnectedProver()

	ctx := stream.Context()
	var proverAddr net.Addr
	p, ok := peer.FromContext(ctx)
	if ok {
		proverAddr = p.Addr
	}
	prover, err := prover.New(stream, proverAddr, a.cfg.ProofStatePollingInterval)
	if err != nil {
		return err
	}

	log.Debugf("Establishing stream connection with prover ID [%s], addr [%s]", prover.ID(), prover.Addr())

	for {
		select {
		case <-a.ctx.Done():
			// server disconnected
			return a.ctx.Err()
		case <-ctx.Done():
			// client disconnected
			return ctx.Err()

		default:
			if !prover.IsIdle() {
				log.Debugf("Prover { ID [%s], addr [%s] } is not idle", prover.ID(), prover.Addr())
				time.Sleep(a.cfg.RetryTime.Duration)
				continue
			}

			_, err := a.tryBuildFinalProof(ctx, prover, nil)
			if err != nil {
				log.Errorf("Error checking proofs to verify: %v", err)
			}

			proofGenerated, err := a.tryAggregateProofs(ctx, prover)
			if err != nil {
				log.Errorf("Error trying to aggregate proofs: %v", err)
			}
			if !proofGenerated {
				proofGenerated, err = a.tryGenerateBatchProof(ctx, prover)
				if err != nil {
					log.Errorf("Error trying to generate proof: %v", err)
				}
			}
			if !proofGenerated {
				// if no proof was generated (aggregated or batch) wait some time before retry
				time.Sleep(a.cfg.RetryTime.Duration)
			} // if proof was generated we retry immediately as probably we have more proofs to process
		}
	}
}

// This function waits to receive a final proof from a prover. Once it receives
// the proof, it performs these steps in order:
// - send the final proof to L1
// - wait for the synchronizer to catch up
// - clean up the cache of recursive proofs
func (a *Aggregator) sendFinalProof() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case msg := <-a.finalProof:
			ctx := a.ctx
			proof := msg.recursiveProof

			log.Infof("Verifying final proof with ethereum smart contract, batches %d-%d", proof.BatchNumber, proof.BatchNumberFinal)

			finalBatch, err := a.State.GetBatchByNumber(ctx, proof.BatchNumberFinal, nil)
			if err != nil {
				log.Errorf("Failed to retrieve batch with number [%d]", proof.BatchNumberFinal)
				a.enableProofVerification()
				continue
			}

			inputs := ethmanTypes.FinalProofInputs{
				FinalProof:       msg.finalProof,
				NewLocalExitRoot: finalBatch.LocalExitRoot.Bytes(),
				NewStateRoot:     finalBatch.StateRoot.Bytes(),
			}

			log.Infof("Final proof inputs: NewLocalExitRoot [%#x], NewStateRoot [%#x]", inputs.NewLocalExitRoot, inputs.NewStateRoot)

			tx, err := a.EthTxManager.VerifyBatches(ctx, proof.BatchNumber-1, proof.BatchNumberFinal, &inputs)
			if err != nil {
				log.Errorf("Error verifiying final proof for batches [%d-%d], err: %v", proof.BatchNumber, proof.BatchNumberFinal, err)

				// unlock the underlying proof (generating=false)
				proof.Generating = false
				err := a.State.UpdateGeneratedProof(ctx, proof, nil)
				if err != nil {
					log.Errorf("Rollback failed updating proof state (false) for proof ID [%v], err: %v", proof.ProofID, err)
				}
				a.enableProofVerification()
				continue
			}

			log.Infof("Final proof for batches [%d-%d] verified in transaction [%v]", proof.BatchNumber, proof.BatchNumberFinal, tx.Hash())

			// wait for the synchronizer to catch up the verified batches
			log.Debug("A final proof has been sent, waiting for the network to be synced")
			for !a.isSynced(a.ctx) {
				log.Info("Waiting for synchronizer to sync...")
				time.Sleep(a.cfg.RetryTime.Duration)
			}

			a.resetVerifyProofTime()

			// network is synced with the final proof, we can safely delete the recursive proofs
			err = a.State.DeleteGeneratedProofs(ctx, proof.BatchNumber, proof.BatchNumberFinal, nil)
			if err != nil {
				log.Errorf("Failed to store proof aggregation result, err: %v", err)
			}
		}
	}
}

// buildFinalProof builds and return the final proof for an aggregated/batch proof.
func (a *Aggregator) buildFinalProof(ctx context.Context, prover proverInterface, proof *state.Proof) (*pb.FinalProof, error) {
	log.Infof("Prover { ID[%s], addr[%s] }  is going to be used to generate final proof for batches [%d-%d]",
		prover.ID(), prover.Addr(), proof.BatchNumber, proof.BatchNumberFinal)

	pubAddr, err := a.Ethman.GetPublicAddress()
	if err != nil {
		return nil, fmt.Errorf("Failed to get public address, %w", err)
	}

	finalProofID, err := prover.FinalProof(proof.Proof, pubAddr.String())
	if err != nil {
		return nil, fmt.Errorf("Failed to get final proof id, %w", err)
	}

	proof.ProofID = finalProofID

	log.Infof("Final proof ID for batches [%d-%d]: %s", proof.BatchNumber, proof.BatchNumberFinal, *proof.ProofID)

	finalProof, err := prover.WaitFinalProof(ctx, *proof.ProofID)
	if err != nil {
		return nil, fmt.Errorf("Failed to get final proof from prover, %w", err)
	}

	log.Infof("Final proof [%s] generated", *proof.ProofID)

	// mock prover sanity check
	if string(finalProof.Public.NewStateRoot) == mockedStateRoot && string(finalProof.Public.NewLocalExitRoot) == mockedLocalExitRoot {
		// This local exit root and state root come from the mock
		// prover, use the one captured by the executor instead
		finalBatch, err := a.State.GetBatchByNumber(ctx, proof.BatchNumberFinal, nil)
		if err != nil {
			return nil, fmt.Errorf("Failed to retrieve batch with number [%d]", proof.BatchNumberFinal)
		}
		log.Warnf("NewLocalExitRoot and NewStateRoot look like a mock values, using values from executor instead: LER: %v, SR: %v",
			finalBatch.LocalExitRoot.TerminalString(), finalBatch.StateRoot.TerminalString())
		finalProof.Public.NewStateRoot = finalBatch.StateRoot.Bytes()
		finalProof.Public.NewLocalExitRoot = finalBatch.LocalExitRoot.Bytes()
	}

	return finalProof, nil
}

// tryBuildFinalProof checks if the provided proof is eligible to be used to
// build the final proof.  If no proof is provided it looks for a previously
// generated proof.  If the proof is eligible, then the final proof generation
// is triggered.
func (a *Aggregator) tryBuildFinalProof(ctx context.Context, prover proverInterface, proof *state.Proof) (bool, error) {
	log.Debugf("tryBuildFinalProof start prover { ID [%s], addr [%s] }", prover.ID(), prover.Addr())

	var err error
	if !a.canVerifyProof() {
		log.Debug("Time to verify proof not reached")
		return false, nil
	}
	log.Debug("Send final proof time reached")

	defer func() {
		if err != nil {
			a.enableProofVerification()
		}
	}()

	for !a.isSynced(ctx) {
		log.Info("Waiting for synchronizer to sync...")
		time.Sleep(a.cfg.RetryTime.Duration)
		continue
	}

	var lastVerifiedBatchNum uint64
	lastVerifiedBatch, err := a.State.GetLastVerifiedBatch(ctx, nil)
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		return false, fmt.Errorf("Failed to get last verified batch, %w", err)
	}
	if lastVerifiedBatch != nil {
		lastVerifiedBatchNum = lastVerifiedBatch.BatchNumber
	}

	if proof == nil {
		// we don't have a proof generating at the moment, check if we
		// have a proof ready to verify

		proof, err = a.getAndLockProofReadyToVerify(ctx, prover, lastVerifiedBatchNum)
		if errors.Is(err, state.ErrNotFound) {
			// nothing to verify, swallow the error
			log.Debug("No proof ready to verify")
			return false, nil
		}
		if err != nil {
			return false, err
		}

		defer func() {
			if err != nil {
				// Set the generating state to false for the proof ("unlock" it)
				proof.Generating = false
				err2 := a.State.UpdateGeneratedProof(a.ctx, proof, nil)
				if err2 != nil {
					log.Errorf("Failed to delete proof in progress, err: %v", err2)
				}
			}
		}()
	} else {
		// we do have a proof generating at the moment, check if it is
		// eligible to be verified

		var eligible bool // we need this to keep using err from the outer scope and trigger the defer func
		eligible, err = a.validateEligibleFinalProof(ctx, proof, lastVerifiedBatchNum)
		if err != nil {
			return false, fmt.Errorf("Failed to validate eligible final proof, %w", err)
		}
		if !eligible {
			return false, nil
		}
	}

	// at this point we have an eligible proof, build the final one using it
	finalProof, err := a.buildFinalProof(ctx, prover, proof)
	if err != nil {
		return false, fmt.Errorf("Failed to build final proof, %w", err)
	}
	if finalProof == nil {
		// If finalProof has not been generated for any reason,
		// generate error and return (this also will unlock the proof to verify)
		err = errors.New("Error generating final proof for proof ready to verify")
		return false, err
	}

	msg := finalProofMsg{
		proverID:       prover.ID(),
		recursiveProof: proof,
		finalProof:     finalProof,
	}

	select {
	case <-a.ctx.Done():
		return false, a.ctx.Err()
	case a.finalProof <- msg:
	}

	log.Debug("tryBuildFinalProof end")
	return true, nil
}

func (a *Aggregator) validateEligibleFinalProof(ctx context.Context, proof *state.Proof, lastVerifiedBatchNum uint64) (bool, error) {
	batchNumberToVerify := lastVerifiedBatchNum + 1

	if proof.BatchNumber != batchNumberToVerify {
		log.Infof("Proof batch number %d is not the following to last verfied batch number %d", proof.BatchNumber, lastVerifiedBatchNum)
		return false, nil
	}

	bComplete, err := a.State.CheckProofContainsCompleteSequences(ctx, proof, nil)
	if err != nil {
		return false, fmt.Errorf("Failed to check if proof contains compete sequences, %w", err)
	}
	if !bComplete {
		log.Infof("Recursive proof %d-%d not eligible to be verified: not containing complete sequences", proof.BatchNumber, proof.BatchNumberFinal)
		return false, nil
	}
	return true, nil
}

func (a *Aggregator) getAndLockProofReadyToVerify(ctx context.Context, prover proverInterface, lastVerifiedBatchNum uint64) (*state.Proof, error) {
	a.StateDBMutex.Lock()
	defer a.StateDBMutex.Unlock()

	// Get proof ready to be verified
	proofToVerify, err := a.State.GetProofReadyToVerify(ctx, lastVerifiedBatchNum, nil)
	if err != nil {
		return nil, err
	}

	proofToVerify.Generating = true

	err = a.State.UpdateGeneratedProof(ctx, proofToVerify, nil)
	if err != nil {
		return nil, err
	}

	return proofToVerify, nil
}

func (a *Aggregator) unlockProofsToAggregate(ctx context.Context, proof1 *state.Proof, proof2 *state.Proof) error {
	// Release proofs from generating state in a single transaction
	dbTx, err := a.State.BeginStateTransaction(ctx)
	if err != nil {
		log.Warnf("Failed to begin transaction to release proof aggregation state, err: %v", err)
		return err
	}

	proof1.Generating = false
	err = a.State.UpdateGeneratedProof(ctx, proof1, dbTx)
	if err == nil {
		proof2.Generating = false
		err = a.State.UpdateGeneratedProof(ctx, proof2, dbTx)
	}

	if err != nil {
		dbTx.Rollback(ctx) //nolint:errcheck
		return fmt.Errorf("Failed to release proof aggregation state %w", err)
	}

	err = dbTx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("Failed to release proof aggregation state %w", err)
	}

	return nil
}

func (a *Aggregator) getAndLockProofsToAggregate(ctx context.Context, prover proverInterface) (*state.Proof, *state.Proof, error) {
	a.StateDBMutex.Lock()
	defer a.StateDBMutex.Unlock()

	proof1, proof2, err := a.State.GetProofsToAggregate(ctx, nil)
	if err != nil {
		return nil, nil, err
	}

	// Set proofs in generating state in a single transaction
	dbTx, err := a.State.BeginStateTransaction(ctx)
	if err != nil {
		log.Errorf("Failed to begin transaction to set proof aggregation state, err: %v", err)
		return nil, nil, err
	}

	proof1.Generating = true
	err = a.State.UpdateGeneratedProof(ctx, proof1, dbTx)
	if err == nil {
		proof2.Generating = true
		err = a.State.UpdateGeneratedProof(ctx, proof2, dbTx)
	}

	if err != nil {
		dbTx.Rollback(ctx) //nolint:errcheck
		return nil, nil, fmt.Errorf("Failed to set proof aggregation state %w", err)
	}

	err = dbTx.Commit(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to set proof aggregation state %w", err)
	}

	return proof1, proof2, nil
}

func (a *Aggregator) tryAggregateProofs(ctx context.Context, prover proverInterface) (bool, error) {
	log.Debugf("tryAggregateProofs start prover { ID [%s], addr [%s] }", prover.ID(), prover.Addr())

	proof1, proof2, err0 := a.getAndLockProofsToAggregate(ctx, prover)
	if errors.Is(err0, state.ErrNotFound) {
		// nothing to aggregate, swallow the error
		log.Debug("Nothing to aggregate")
		return false, nil
	}
	if err0 != nil {
		return false, err0
	}

	var err error

	defer func() {
		if err != nil {
			err2 := a.unlockProofsToAggregate(a.ctx, proof1, proof2)
			if err2 != nil {
				log.Errorf("Failed to release aggregated proofs, err: %v", err2)
			}
		}
		log.Debug("tryAggregateProofs end")
	}()

	log.Infof("Prover { ID [%s], addr [%s] } is going to be used to aggregate proofs: %d-%d and %d-%d",
		prover.ID(), prover.Addr(), proof1.BatchNumber, proof1.BatchNumberFinal, proof2.BatchNumber, proof2.BatchNumberFinal)

	proverID := prover.ID()
	inputProver := map[string]interface{}{
		"recursive_proof_1": proof1.Proof,
		"recursive_proof_2": proof2.Proof,
	}
	b, err := json.Marshal(inputProver)
	if err != nil {
		return false, fmt.Errorf("Failed to serialize input prover, %w", err)
	}

	proof := &state.Proof{
		BatchNumber:      proof1.BatchNumber,
		BatchNumberFinal: proof2.BatchNumberFinal,
		Prover:           &proverID,
		InputProver:      string(b),
		Generating:       true,
	}

	aggrProofID, err := prover.AggregatedProof(proof1.Proof, proof2.Proof)
	if err != nil {
		return false, fmt.Errorf("Failed to get aggregated proof id, %w", err)
	}

	proof.ProofID = aggrProofID

	log.Infof("Proof ID for aggregated proof %d-%d: %v", proof.BatchNumber, proof.BatchNumberFinal, *proof.ProofID)

	recursiveProof, err := prover.WaitRecursiveProof(ctx, *proof.ProofID)
	if err != nil {
		return false, fmt.Errorf("Failed to get aggregated proof from prover, %w", err)
	}

	log.Infof("Aggregated proof %s generated", *proof.ProofID)

	proof.Proof = recursiveProof

	// update the state by removing the 2 aggregated proofs and storing the
	// newly generated recursive proof
	dbTx, err := a.State.BeginStateTransaction(ctx)
	if err != nil {
		return false, fmt.Errorf("Failed to begin transaction to update proof aggregation state %w", err)
	}

	err = a.State.DeleteGeneratedProofs(ctx, proof1.BatchNumber, proof2.BatchNumberFinal, dbTx)
	if err != nil {
		dbTx.Rollback(ctx) //nolint:errcheck
		return false, fmt.Errorf("Failed to delete previously aggregated proofs %w", err)
	}
	err = a.State.AddGeneratedProof(ctx, proof, dbTx)
	if err != nil {
		dbTx.Rollback(ctx) //nolint:errcheck
		return false, fmt.Errorf("Failed to store the recursive proof %w", err)
	}

	err = dbTx.Commit(ctx)
	if err != nil {
		return false, fmt.Errorf("Failed to store the recursive proof %w", err)
	}

	// state is up to date, check if we can send the final proof using the
	// one just crafted.
	finalProofBuilt, err := a.tryBuildFinalProof(ctx, prover, proof)
	if err != nil {
		return false, fmt.Errorf("Failed trying to check if recursive proof can be verified: %w", err)
	}

	// NOTE(pg): prover is done, use a.ctx from now on

	if !finalProofBuilt {
		proof.Generating = false

		// final proof has not been generated, update the recursive proof
		err = a.State.UpdateGeneratedProof(a.ctx, proof, nil)
		if err != nil {
			log.Errorf("Failed to store batch proof result, err %v", err)
			return false, err
		}
	}

	return true, nil
}

func (a *Aggregator) getAndLockBatchToProve(ctx context.Context, prover *prover.Prover) (*state.Batch, *state.Proof, error) {
	a.StateDBMutex.Lock()
	defer a.StateDBMutex.Unlock()

	lastVerifiedBatch, err := a.State.GetLastVerifiedBatch(ctx, nil)
	if err != nil {
		return nil, nil, err
	}

	// Get virtual batch pending to generate proof
	batchToVerify, err := a.State.GetVirtualBatchToProve(ctx, lastVerifiedBatch.BatchNumber, nil)
	if err != nil {
		return nil, nil, err
	}

	log.Infof("Found virtual batch %d pending to generate proof", batchToVerify.BatchNumber)

	log.Infof("Checking profitability to aggregate batch, batchNumber: %d", batchToVerify.BatchNumber)

	// pass matic collateral as zero here, bcs in smart contract fee for aggregator is not defined yet
	isProfitable, err := a.ProfitabilityChecker.IsProfitable(ctx, big.NewInt(0))
	if err != nil {
		log.Errorf("Failed to check aggregator profitability, err: %v", err)
		return nil, nil, err
	}

	if !isProfitable {
		log.Infof("Batch %d is not profitable, matic collateral %d", batchToVerify.BatchNumber, big.NewInt(0))
		return nil, nil, err
	}

	proverID := prover.ID()
	proof := &state.Proof{
		BatchNumber:      batchToVerify.BatchNumber,
		BatchNumberFinal: batchToVerify.BatchNumber,
		Prover:           &proverID,
		Generating:       true,
	}

	// Avoid other prover to process the same batch
	err = a.State.AddGeneratedProof(ctx, proof, nil)
	if err != nil {
		log.Errorf("Failed to add batch proof, err: %v", err)
		return nil, nil, err
	}

	return batchToVerify, proof, nil
}

func (a *Aggregator) tryGenerateBatchProof(ctx context.Context, prover *prover.Prover) (bool, error) {
	log.Debugf("tryGenerateBatchProof start prover { ID [%s], addr [%s] }", prover.ID(), prover.Addr())

	batchToProve, proof, err0 := a.getAndLockBatchToProve(ctx, prover)
	if errors.Is(err0, state.ErrNotFound) {
		// nothing to proof, swallow the error
		log.Debug("Nothing to generate proof")
		return false, nil
	}
	if err0 != nil {
		return false, err0
	}

	var err error

	defer func() {
		if err != nil {
			err2 := a.State.DeleteGeneratedProofs(a.ctx, proof.BatchNumber, proof.BatchNumberFinal, nil)
			if err2 != nil {
				log.Errorf("Failed to delete proof in progress, err: %v", err2)
			}
		}
		log.Debug("tryGenerateBatchProof end")
	}()

	log.Infof("Prover { ID [%s], addr [%s] } is going to be used to generate proof from batch [%d]", prover.ID(), prover.Addr(), batchToProve.BatchNumber)

	log.Infof("Sending zki + batch to the prover, batchNumber [%d]", batchToProve.BatchNumber)
	inputProver, err := a.buildInputProver(ctx, batchToProve)
	if err != nil {
		return false, fmt.Errorf("Failed to build input prover, %w", err)
	}

	b, err := json.Marshal(inputProver)
	if err != nil {
		return false, fmt.Errorf("Failed to serialize input prover, %w", err)
	}

	proof.InputProver = string(b)

	log.Infof("Sending a batch to the prover. OldStateRoot [%#x], OldBatchNum [%d]",
		inputProver.PublicInputs.OldStateRoot, inputProver.PublicInputs.OldBatchNum)

	genProofID, err := prover.BatchProof(inputProver)
	if err != nil {
		return false, fmt.Errorf("Failed to get batch proof id %w", err)
	}

	proof.ProofID = genProofID

	log.Infof("Proof ID for batch %d: %v", proof.BatchNumber, *proof.ProofID)

	resGetProof, err := prover.WaitRecursiveProof(ctx, *proof.ProofID)
	if err != nil {
		return false, fmt.Errorf("Failed to get proof from prover %w", err)
	}

	log.Infof("Batch proof %s generated", *proof.ProofID)

	proof.Proof = resGetProof

	finalProofBuilt, err := a.tryBuildFinalProof(ctx, prover, proof)
	if err != nil {
		return false, fmt.Errorf("Failed trying to build final proof %w", err)
	}

	// NOTE(pg): prover is done, use a.ctx from now on

	if !finalProofBuilt {
		proof.Generating = false

		// final proof has not been generated, update the recursive proof
		err = a.State.UpdateGeneratedProof(a.ctx, proof, nil)
		if err != nil {
			log.Errorf("Failed to store batch proof result, err %v", err)
			return false, err
		}
	}

	return true, nil
}

// canVerifyProof returns true if we have reached the timeout to verify a proof
// and no other prover is verifying a proof.
func (a *Aggregator) canVerifyProof() bool {
	a.TimeSendFinalProofMutex.Lock()
	defer a.TimeSendFinalProofMutex.Unlock()
	if a.TimeSendFinalProof.Before(time.Now()) {
		if a.verifyingProof {
			return false
		}
		a.verifyingProof = true
		return true
	}
	return false
}

func (a *Aggregator) enableProofVerification() {
	a.TimeSendFinalProofMutex.Lock()
	defer a.TimeSendFinalProofMutex.Unlock()
	a.verifyingProof = false
}

// resetVerifyProofTime updates the timeout to verify a proof.
func (a *Aggregator) resetVerifyProofTime() {
	a.TimeSendFinalProofMutex.Lock()
	defer a.TimeSendFinalProofMutex.Unlock()
	a.verifyingProof = false
	a.TimeSendFinalProof = time.Now().Add(a.cfg.VerifyProofInterval.Duration)
}

func (a *Aggregator) isSynced(ctx context.Context) bool {
	lastVerifiedBatch, err := a.State.GetLastVerifiedBatch(ctx, nil)
	if err != nil && err != state.ErrNotFound {
		log.Warnf("Failed to get last consolidated batch, err: %v", err)
		return false
	}
	if lastVerifiedBatch == nil {
		return false
	}
	lastVerifiedEthBatchNum, err := a.Ethman.GetLatestVerifiedBatchNum()
	if err != nil {
		log.Warnf("Failed to get last eth batch, err: %v", err)
		return false
	}
	if lastVerifiedBatch.BatchNumber < lastVerifiedEthBatchNum {
		log.Infof("Waiting for the state to be synced, lastVerifiedBatchNum: %d, lastVerifiedEthBatchNum: %d",
			lastVerifiedBatch.BatchNumber, lastVerifiedEthBatchNum)
		return false
	}
	return true
}

func (a *Aggregator) buildInputProver(ctx context.Context, batchToVerify *state.Batch) (*pb.InputProver, error) {
	previousBatch, err := a.State.GetBatchByNumber(ctx, batchToVerify.BatchNumber-1, nil)
	if err != nil && err != state.ErrStateNotSynchronized {
		return nil, fmt.Errorf("Failed to get previous batch, err: %v", err)
	}

	pubAddr, err := a.Ethman.GetPublicAddress()
	if err != nil {
		return nil, fmt.Errorf("failed to get public address, err: %w", err)
	}

	inputProver := &pb.InputProver{
		PublicInputs: &pb.PublicInputs{
			OldStateRoot:    previousBatch.StateRoot.Bytes(),
			OldAccInputHash: previousBatch.AccInputHash.Bytes(),
			OldBatchNum:     previousBatch.BatchNumber,
			ChainId:         a.cfg.ChainID,
			BatchL2Data:     batchToVerify.BatchL2Data,
			GlobalExitRoot:  batchToVerify.GlobalExitRoot.Bytes(),
			EthTimestamp:    uint64(batchToVerify.Timestamp.Unix()),
			SequencerAddr:   batchToVerify.Coinbase.String(),
			AggregatorAddr:  pubAddr.String(),
		},
		Db:                map[string]string{},
		ContractsBytecode: map[string]string{},
	}

	return inputProver, nil
}

// healthChecker will provide an implementation of the HealthCheck interface.
type healthChecker struct{}

// newHealthChecker returns a health checker according to standard package
// grpc.health.v1.
func newHealthChecker() *healthChecker {
	return &healthChecker{}
}

// HealthCheck interface implementation.

// Check returns the current status of the server for unary gRPC health requests,
// for now if the server is up and able to respond we will always return SERVING.
func (hc *healthChecker) Check(ctx context.Context, req *grpchealth.HealthCheckRequest) (*grpchealth.HealthCheckResponse, error) {
	log.Info("Serving the Check request for health check")
	return &grpchealth.HealthCheckResponse{
		Status: grpchealth.HealthCheckResponse_SERVING,
	}, nil
}

// Watch returns the current status of the server for stream gRPC health requests,
// for now if the server is up and able to respond we will always return SERVING.
func (hc *healthChecker) Watch(req *grpchealth.HealthCheckRequest, server grpchealth.Health_WatchServer) error {
	log.Info("Serving the Watch request for health check")
	return server.Send(&grpchealth.HealthCheckResponse{
		Status: grpchealth.HealthCheckResponse_SERVING,
	})
}
