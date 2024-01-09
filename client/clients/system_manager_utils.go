package clients

import (
	"crypto/ecdsa"
	"flare-tlc/client/shared"
	"flare-tlc/database"
	"flare-tlc/logger"
	"flare-tlc/utils"
	"flare-tlc/utils/chain"
	"flare-tlc/utils/contracts/system"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

type SystemManagerContractClient struct {
	address            common.Address
	flareSystemManager *system.FlareSystemManager
	txOpts             *bind.TransactOpts
	txVerifier         *chain.TxVerifier
	privateKey         *ecdsa.PrivateKey
}

func NewSystemManagerClient(
	chainID int,
	ethClient *ethclient.Client,
	address common.Address,
	privateKeyString string,
) (*SystemManagerContractClient, error) {
	txOpts, privateKey, err := chain.CredentialsFromPrivateKey(privateKeyString, chainID)
	if err != nil {
		return nil, err
	}
	flareSystemManager, err := system.NewFlareSystemManager(address, ethClient)
	if err != nil {
		return nil, err
	}

	return &SystemManagerContractClient{
		address:            address,
		flareSystemManager: flareSystemManager,
		txOpts:             txOpts,
		txVerifier:         chain.NewTxVerifier(ethClient),
		privateKey:         privateKey,
	}, nil
}

func (s *SystemManagerContractClient) SignNewSigningPolicy(rewardEpochId *big.Int, signingPolicy []byte) <-chan ExecuteStatus {
	return ExecuteWithRetry(func() error {
		err := s.sendSignNewSigningPolicy(rewardEpochId, signingPolicy)
		if err != nil {
			return errors.Wrap(err, "error sending sign new signing policy")
		}
		return nil
	}, MaxTxSendRetries)
}

func (s *SystemManagerContractClient) sendSignNewSigningPolicy(rewardEpochId *big.Int, signingPolicy []byte) error {
	newSigningPolicyHash := SigningPolicyHash(signingPolicy)
	hashSignature, err := crypto.Sign(accounts.TextHash(newSigningPolicyHash), s.privateKey)
	if err != nil {
		return err
	}

	signature := system.FlareSystemManagerSignature{
		V: hashSignature[0],
		R: [32]byte(hashSignature[1:33]),
		S: [32]byte(hashSignature[33:65]),
	}

	tx, err := s.flareSystemManager.SignNewSigningPolicy(s.txOpts, rewardEpochId, [32]byte(newSigningPolicyHash), signature)
	if err != nil {
		return err
	}
	err = s.txVerifier.WaitUntilMined(s.txOpts.From, tx, chain.DefaultTxTimeout)
	if err != nil {
		return err
	}
	logger.Info("New signing policy sent for epoch %v", rewardEpochId)
	return nil
}

func SigningPolicyHash(signingPolicy []byte) []byte {
	if len(signingPolicy)%32 != 0 {
		signingPolicy = append(signingPolicy, make([]byte, 32-len(signingPolicy)%32)...)
	}
	hash := crypto.Keccak256(signingPolicy[:32], signingPolicy[32:64])
	for i := 2; i < len(signingPolicy)/32; i++ {
		hash = crypto.Keccak256(hash, signingPolicy[i*32:(i+1)*32])
	}
	return hash
}

func (s *SystemManagerContractClient) VotePowerBlockSelectedListener(db *gorm.DB, epoch *utils.Epoch) <-chan *system.FlareSystemManagerVotePowerBlockSelected {
	out := make(chan *system.FlareSystemManagerVotePowerBlockSelected)
	topic0, err := chain.EventIDFromMetadata(system.FlareSystemManagerMetaData, "VotePowerBlockSelected")
	if err != nil {
		// panic, this error is fatal
		panic(err)
	}
	go func() {
		ticker := time.NewTicker(ListenerInterval)
		eventRangeStart := epoch.StartTime(epoch.EpochIndex(time.Now()) - 1).Unix()
		for {
			<-ticker.C
			now := time.Now().Unix()
			logs, err := database.FetchLogsByAddressAndTopic0(db, s.address.Hex(), topic0, eventRangeStart, now)
			if err != nil {
				logger.Error("Error fetching logs %v", err)
				continue
			}
			if len(logs) > 0 {
				powerBlockData, err := s.parseVotePowerBlockSelectedEvent(logs[len(logs)-1])
				if err != nil {
					logger.Error("Error parsing VotePowerBlockSelected event %v", err)
					continue
				}
				out <- powerBlockData
			}
		}
	}()
	return out
}

func (s *SystemManagerContractClient) parseVotePowerBlockSelectedEvent(dbLog database.Log) (*system.FlareSystemManagerVotePowerBlockSelected, error) {
	contractLog, err := shared.ConvertDatabaseLogToChainLog(dbLog)
	if err != nil {
		return nil, err
	}
	return s.flareSystemManager.FlareSystemManagerFilterer.ParseVotePowerBlockSelected(*contractLog)
}

func (s *SystemManagerContractClient) EpochFromChain() (*utils.Epoch, error) {
	epochStart, err := s.flareSystemManager.RewardEpochsStartTs(nil)
	if err != nil {
		return nil, err
	}
	epochPeriod, err := s.flareSystemManager.RewardEpochDurationSeconds(nil)
	if err != nil {
		return nil, err
	}
	return &utils.Epoch{
		Start:  time.Unix(int64(epochStart), 0),
		Period: time.Duration(epochPeriod) * time.Second,
	}, nil
}
