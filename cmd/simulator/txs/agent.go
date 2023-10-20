// (c) 2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package txs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/ava-labs/subnet-evm/cmd/simulator/metrics"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

type THash interface {
	Hash() common.Hash
}

// TxSequence provides an interface to return a channel of transactions.
// The sequence is responsible for closing the channel when there are no further
// transactions.
type TxSequence[T THash] interface {
	Chan() <-chan T
}

// Worker defines the interface for issuance and confirmation of transactions.
// The caller is responsible for calling Close to cleanup resources used by the
// worker at the end of the simulation.
type Worker[T THash] interface {
	IssueTx(ctx context.Context, tx T) error
	ConfirmTx(ctx context.Context, tx T) error
	Close(ctx context.Context) error
	From(ctx context.Context) common.Address
	ConfirmErrors(ctx context.Context) int
}

// Execute the work of the given agent.
type Agent[T THash] interface {
	Execute(ctx context.Context) error
	TPS(ctx context.Context) float64
	Status(ctx context.Context) Status
	Progress(ctx context.Context) int
	IssueErrors(ctx context.Context) int
	ConfirmErrors(ctx context.Context) int
	Error(ctx context.Context) error
}

type Status int

const (
	Active Status = iota
	Completed
	Errored
)

// issueNAgent issues and confirms a batch of N transactions at a time.
type issueNAgent[T THash] struct {
	sequence       TxSequence[T]
	worker         Worker[T]
	n              uint64
	setTps         float64
	curTps         float64
	status         Status
	confirmedCount int
	metrics        *metrics.Metrics
	issueErrors    int
	err            error
}

// NewIssueNAgent creates a new issueNAgent
func NewIssueNAgent[T THash](sequence TxSequence[T], worker Worker[T], n uint64, tps float64, metrics *metrics.Metrics) Agent[T] {
	return &issueNAgent[T]{
		sequence:       sequence,
		worker:         worker,
		n:              n,
		setTps:         tps,
		curTps:         0.,
		status:         Active,
		confirmedCount: 0,
		metrics:        metrics,
		issueErrors:    0,
		err:            nil,
	}
}

// Execute issues txs in batches of N and waits for them to confirm
func (a *issueNAgent[T]) Execute(ctx context.Context) error {
	if a.n == 0 {
		return errors.New("batch size n cannot be equal to 0")
	}

	txChan := a.sequence.Chan()
	confirmedCount := 0
	batchI := 0
	m := a.metrics
	txMap := make(map[common.Hash]time.Time)

	// Tracks the total amount of time waiting for issuing and confirming txs
	var (
		totalIssuedTime    time.Duration
		totalConfirmedTime time.Duration
	)

	defer func() {
		if err := a.worker.Close(ctx); err != nil {
			log.Error("error trying to close worker: %w", "err", err)
		}
	}()

	// Start time for execution
	start := time.Now()
	errorCount := uint64(0)
	for {
		var (
			txs     = make([]T, 0, a.n)
			tx      T
			moreTxs bool
		)
		// Start issuance batch
		issuedStart := time.Now()
	L:
		for i := uint64(0); i < a.n; i++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case tx, moreTxs = <-txChan:
				if !moreTxs {
					break L
				}
				issuanceIndividualStart := time.Now()
				txMap[tx.Hash()] = issuanceIndividualStart
				for j := uint64(0); j < 5; j++ {
					if err := a.worker.IssueTx(ctx, tx); err != nil && err.Error() != "already known" {
						a.issueErrors++
						errorCount++
						if errorCount > 15 {
							log.Info("Issuance Error Worker Offline", "Address", a.worker.From(ctx), "Hash", tx.Hash(), "Error", err)
							a.status = Errored
							return fmt.Errorf("%q failed to issue %d consecutive txs: Last Error = %w", a.worker.From(ctx), 15, err)
							// return fmt.Errorf("%q failed to issue transaction %d: %w", a.worker.From(ctx), len(txs), err)
						}
					} else {
						errorCount = 0
						j = 5
					}
				}
				txs = append(txs, tx)
				issuanceIndividualDuration := time.Since(issuanceIndividualStart)
				m.IssuanceTxTimes.Observe(issuanceIndividualDuration.Seconds())
			}
		}
		// Get the batch's issuance time and add it to totalIssuedTime
		issuedDuration := time.Since(issuedStart)
		// log.Info("Issuance Batch Done", "batch", batchI, "time", issuedDuration.Seconds())
		totalIssuedTime += issuedDuration

		// Wait for txs in this batch to confirm
		confirmedStart := time.Now()
		for i, tx := range txs {
			confirmedIndividualStart := time.Now()
			if err := a.worker.ConfirmTx(ctx, tx); err != nil {
				log.Info("Confirmation Error", "Address", a.worker.From(ctx), "Hash", tx.Hash(), "Error", err)
				a.status = Errored
				return fmt.Errorf("failed to await transaction %d: %w", i, err)
			}
			confirmationIndividualDuration := time.Since(confirmedIndividualStart)
			issuanceToConfirmationIndividualDuration := time.Since(txMap[tx.Hash()])
			m.ConfirmationTxTimes.Observe(confirmationIndividualDuration.Seconds())
			m.IssuanceToConfirmationTxTimes.Observe(issuanceToConfirmationIndividualDuration.Seconds())
			delete(txMap, tx.Hash())
			confirmedCount++
		}
		a.confirmedCount = confirmedCount
		// Get the batch's confirmation time and add it to totalConfirmedTime
		confirmedDuration := time.Since(confirmedStart)
		// log.Info("Confirmed Batch Done", "batch", batchI, "time", confirmedDuration.Seconds())
		totalConfirmedTime += confirmedDuration
		a.curTps = float64(confirmedCount) / time.Since(start).Seconds()
		// Check if this is the last batch, if so write the final log and return
		if !moreTxs {
			a.status = Completed
			totalTime := time.Since(start).Seconds()
			log.Info("Execution complete", "totalTxs", confirmedCount, "totalTime", totalTime, "TPS", float64(confirmedCount)/totalTime,
				"issuanceTime", totalIssuedTime.Seconds(), "confirmedTime", totalConfirmedTime.Seconds())

			return nil
		}

		if a.setTps != 0 {
			batchTime := float64(a.n) / a.setTps
			diff := batchTime - (issuedDuration.Seconds() + confirmedDuration.Seconds())
			waitTime := math.Min(4, diff)
			time.Sleep(time.Duration(waitTime * float64(time.Second)))
		}
		batchI++
	}
}

func (a *issueNAgent[T]) Progress(ctx context.Context) int {
	return a.confirmedCount
}

func (a *issueNAgent[T]) TPS(ctx context.Context) float64 {
	return a.curTps
}

func (a *issueNAgent[T]) Status(ctx context.Context) Status {
	return a.status
}

func (a *issueNAgent[T]) IssueErrors(ctx context.Context) int {
	return a.issueErrors
}

func (a *issueNAgent[T]) ConfirmErrors(ctx context.Context) int {
	return a.worker.ConfirmErrors(ctx)
}

func (a *issueNAgent[T]) Error(ctx context.Context) error {
	return a.err
}
