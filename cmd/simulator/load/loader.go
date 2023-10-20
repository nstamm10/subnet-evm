// Copyright (C) 2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package load

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ava-labs/subnet-evm/cmd/simulator/config"
	"github.com/ava-labs/subnet-evm/cmd/simulator/key"
	"github.com/ava-labs/subnet-evm/cmd/simulator/metrics"
	"github.com/ava-labs/subnet-evm/cmd/simulator/txs"
	"github.com/ava-labs/subnet-evm/core/types"
	"github.com/ava-labs/subnet-evm/ethclient"
	"github.com/ava-labs/subnet-evm/params"
	"github.com/ethereum/go-ethereum/common"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"
)

const (
	MetricsEndpoint = "/metrics" // Endpoint for the Prometheus Metrics Server
)

// Key value structure for error map
type kv struct {
	Error string
	Count int
}

// ExecuteLoader creates txSequences from [config] and has txAgents execute the specified simulation.
func ExecuteLoader(ctx context.Context, config config.Config) error {
	if config.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, config.Timeout)
		defer cancel()
	}

	// Create buffered sigChan to receive SIGINT notifications
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT)

	// Create context with cancel
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		// Blocks until we receive a SIGINT notification or if parent context is done
		select {
		case <-sigChan:
		case <-ctx.Done():
		}

		// Cancel the child context and end all processes
		cancel()
	}()

	// Construct the arguments for the load simulator
	clients := make([]ethclient.Client, 0, len(config.Endpoints))
	for i := 0; i < config.Workers; i++ {
		clientURI := config.Endpoints[i%len(config.Endpoints)]
		client, err := ethclient.Dial(clientURI)
		if err != nil {
			return fmt.Errorf("failed to dial client at %s: %w", clientURI, err)
		}
		clients = append(clients, client)
	}

	keys, err := key.LoadAll(ctx, config.KeyDir)
	if err != nil {
		return err
	}
	// Ensure there are at least [config.Workers] keys and save any newly generated ones.
	if len(keys) < config.Workers {
		for i := 0; len(keys) < config.Workers; i++ {
			newKey, err := key.Generate()
			if err != nil {
				return fmt.Errorf("failed to generate %d new key: %w", i, err)
			}
			if err := newKey.Save(config.KeyDir); err != nil {
				return fmt.Errorf("failed to save %d new key: %w", i, err)
			}
			keys = append(keys, newKey)
		}
	}

	// Each address needs: params.GWei * MaxFeeCap * params.TxGas * TxsPerWorker total wei
	// to fund gas for all of their transactions.
	maxFeeCap := new(big.Int).Mul(big.NewInt(params.GWei), big.NewInt(config.MaxFeeCap))
	minFundsPerAddr := new(big.Int).Mul(maxFeeCap, big.NewInt(int64(config.TxsPerWorker*params.TxGas)))

	// Create metrics
	reg := prometheus.NewRegistry()
	m := metrics.NewMetrics(reg)
	metricsPort := strconv.Itoa(int(config.MetricsPort))

	log.Info("Distributing funds", "numTxsPerWorker", config.TxsPerWorker, "minFunds", minFundsPerAddr)
	keys, err = DistributeFunds(ctx, clients[0], keys, config.Workers, minFundsPerAddr, m)
	if err != nil {
		return err
	}
	log.Info("Distributed funds successfully")

	pks := make([]*ecdsa.PrivateKey, 0, len(keys))
	senders := make([]common.Address, 0, len(keys))
	for _, key := range keys {
		pks = append(pks, key.PrivKey)
		senders = append(senders, key.Address)
	}

	bigGwei := big.NewInt(params.GWei)
	gasTipCap := new(big.Int).Mul(bigGwei, big.NewInt(config.MaxTipCap))
	gasFeeCap := new(big.Int).Mul(bigGwei, big.NewInt(config.MaxFeeCap))
	client := clients[0]
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch chainID: %w", err)
	}
	signer := types.LatestSignerForChainID(chainID)

	log.Info("Creating transaction sequences...")
	txGenerator := func(key *ecdsa.PrivateKey, nonce uint64) (*types.Transaction, error) {
		addr := ethcrypto.PubkeyToAddress(key.PublicKey)
		if len(config.SendAddress) != 0 {
			addr = common.HexToAddress(config.SendAddress)
		}

		txData := common.FromHex(config.TxData)
		if config.TxData == "" {
			txData = nil
		}
		tx, err := types.SignNewTx(key, signer, &types.DynamicFeeTx{
			ChainID:   chainID,
			Nonce:     nonce,
			GasTipCap: gasTipCap,
			GasFeeCap: gasFeeCap,
			Gas:       config.GasLimit,
			To:        &addr,
			Data:      txData,
			Value:     common.Big0,
		})
		if err != nil {
			return nil, err
		}
		return tx, nil
	}
	txSequences, err := txs.GenerateTxSequences(ctx, txGenerator, clients[0], pks, config.TxsPerWorker)
	if err != nil {
		return err
	}

	log.Info("Constructing tx agents...", "numAgents", config.Workers)
	agents := make([]txs.Agent[*types.Transaction], 0, config.Workers)
	for i := 0; i < config.Workers; i++ {
		agents = append(agents, txs.NewIssueNAgent[*types.Transaction](txSequences[i], NewSingleAddressTxWorker(ctx, clients[i], senders[i]), config.BatchSize, config.SustainedTps, m))
	}

	log.Info("Starting tx agents...")
	eg := errgroup.Group{}
	for _, agent := range agents {
		agent := agent
		eg.Go(func() error {
			return agent.Execute(ctx)
		})
	}

	go startMetricsServer(ctx, metricsPort, reg)

	log.Info("Waiting for tx agents...")
	waitChan := make(chan struct{})
	go func() {
		eg.Wait()
		close(waitChan)
	}()

L:
	for {
		select {
		case <-waitChan:
			if err := eg.Wait(); err != nil {
				errMap := make(map[string]int)
				for _, agent := range agents {
					errMap[agent.Error(ctx).Error()]++
				}

				var errors []kv
				for e, c := range errMap {
					errors = append(errors, kv{e, c})
				}

				sort.Slice(errors, func(i, j int) bool {
					return errors[i].Count > errors[j].Count
				})
				mostCommonError := errors[0]
				msgStr := fmt.Sprintf("%d errors had this error", mostCommonError.Count)
				log.Info(msgStr, "Error", mostCommonError.Error)
				return nil
			}
			break L
		default:
			time.Sleep(2 * time.Minute)
			var activeAgents, completedAgents, erroredAgents, progress, txRemaining, issueErrors, confirmErrors int
			var tps float64
			for _, agent := range agents {
				tps += agent.TPS(ctx)
				progress += agent.Progress(ctx)
				txRemaining += int(config.TxsPerWorker)
				if agent.Status(ctx) == txs.Active {
					activeAgents++
				}
				if agent.Status(ctx) == txs.Completed {
					completedAgents++
				}
				if agent.Status(ctx) == txs.Errored {
					erroredAgents++
				}
				issueErrors += agent.IssueErrors(ctx)
				confirmErrors += agent.ConfirmErrors(ctx)
			}
			progressString := fmt.Sprintf("%d/%d", progress, txRemaining)
			log.Info("Ongoing Simulation Report", "Progress", progressString, "Current TPS", tps, "Signers Active", activeAgents, "Signers Complete", completedAgents, "Signers Errored", erroredAgents)
			log.Info("Ongoing Error Report", "Issue Errors", issueErrors, "Confirm Errors", confirmErrors)
		}
	}
	// if err := eg.Wait(); err != nil {
	// 	return err
	// }
	log.Info("Tx agents completed successfully.")

	printOutputFromMetricsServer(metricsPort)
	return nil
}

func startMetricsServer(ctx context.Context, metricsPort string, reg *prometheus.Registry) {
	// Create a prometheus server to expose individual tx metrics
	server := &http.Server{
		Addr: fmt.Sprintf(":%s", metricsPort),
	}

	// Start up go routine to listen for SIGINT notifications to gracefully shut down server
	go func() {
		// Blocks until signal is received
		<-ctx.Done()

		if err := server.Shutdown(ctx); err != nil {
			log.Error("Metrics server error: %v", err)
		}
		log.Info("Received a SIGINT signal: Gracefully shutting down metrics server")
	}()

	// Start metrics server
	http.Handle(MetricsEndpoint, promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	log.Info(fmt.Sprintf("Metrics Server: localhost:%s%s", metricsPort, MetricsEndpoint))
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Error("Metrics server error: %v", err)
	}
}

func printOutputFromMetricsServer(metricsPort string) {
	// Get response from server
	resp, err := http.Get(fmt.Sprintf("http://localhost:%s%s", metricsPort, MetricsEndpoint))
	if err != nil {
		log.Error("cannot get response from metrics servers", "err", err)
		return
	}
	// Read response body
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Error("cannot read response body", "err", err)
		return
	}
	// Print out formatted individual metrics
	parts := strings.Split(string(respBody), "\n")
	for _, s := range parts {
		fmt.Printf("       \t\t\t%s\n", s)
	}
}
