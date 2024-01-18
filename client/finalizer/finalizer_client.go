package finalizer

import (
	clientContext "flare-tlc/client/context"
	"flare-tlc/config"
	"flare-tlc/logger"
	"flare-tlc/utils/chain"
	"fmt"
	"time"

	"github.com/pkg/errors"
	"gorm.io/gorm"
)

const (
	startOffset = 7 * 24 * time.Hour // 7 days
)

type finalizerClient struct {
	db *gorm.DB

	relayClient          *relayContractClient
	submissionClient     *submissionContractClient
	signingPolicyStorage *signingPolicyStorage
	submissionStorage    *submissionStorage
	queueProcessor       *finalizerQueueProcessor
}

func NewFinalizerClient(ctx clientContext.ClientContext) (*finalizerClient, error) {
	cfg := ctx.Config()
	if !cfg.Voting.EnabledFinalizer {
		return nil, nil
	}

	chainCfg := cfg.ChainConfig()
	ethClient, err := chainCfg.DialETH()
	if err != nil {
		return nil, err
	}

	senderPkString, err := config.ReadFileToString(cfg.Credentials.SigningPolicyPrivateKeyFile)
	if err != nil {
		return nil, errors.Wrap(err, "error reading sender private key")
	}
	senderPk, err := chain.PrivateKeyFromHex(senderPkString)
	if err != nil {
		return nil, errors.Wrap(err, "error creating sender register tx opts")
	}
	relayClient, err := NewRelayContractClient(
		ethClient,
		cfg.ContractAddresses.Relay,
		senderPk,
	)
	if err != nil {
		return nil, err
	}
	submissionClient := NewSubmissionContractClient(cfg.ContractAddresses.Submission)
	submissionStorage := newSubmissionStorage()

	return &finalizerClient{
		db:                   ctx.DB(),
		relayClient:          relayClient,
		signingPolicyStorage: newSigningPolicyStorage(),
		submissionStorage:    submissionStorage,
		submissionClient:     submissionClient,
		queueProcessor:       newFinalizerQueueProcessor(submissionStorage, relayClient),
	}, nil
}

func (c *finalizerClient) Run() error {
	startTime := time.Now().Add(-startOffset)
	spListener := c.relayClient.SigningPolicyInitializedListener(c.db, startTime)
	go func() {
		for {
			dbPolicy := <-spListener
			// Todo: move to listener to avoid creating a channel and a goroutine
			// Todo: synchronize with the epoch
			c.signingPolicyStorage.Add(newSigningPolicy(dbPolicy.policyData))
		}
	}()
	go func() {
		c.submissionClient.SubmissionTxListener(c.db, startTime, c)
	}()
	go func() {
		c.queueProcessor.Run()
	}()
	return nil
}

func (c *finalizerClient) ProcessSubmissionData(slr submissionListenerResponse) error {
	for _, payloadItem := range slr.payload {
		sp := c.signingPolicyStorage.GetForVotingRound(payloadItem.votingRoundId)
		if sp == nil {
			return fmt.Errorf("no signing policy found for voting round %d", payloadItem.votingRoundId)
		}
		addResult, err := c.submissionStorage.Add(payloadItem.payload, sp)
		if err != nil {
			// Error is non-fatal, skip this submission
			logger.Warn("Error adding submission %v", err)
			continue
		}
		if addResult.thresholdReached {
			logger.Info("Threshold reached for voting round %d", payloadItem.votingRoundId)
			c.queueProcessor.Add(payloadItem)
		}
	}
	return nil
}
