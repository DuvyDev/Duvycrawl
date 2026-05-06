package scorer

import (
	"log/slog"
	"math"

	"github.com/DuvyDev/Duvycrawl/internal/queue"
)

// StaticScorer implements a fixed scoring strategy that mirrors the legacy
// integer priority system. It is used when adaptive scoring is disabled
// or when not enough queries have been recorded to build a user profile.
type StaticScorer struct {
	seedBonus     float64
	normalScore   float64
	recrawlScore  float64
	depthPenaltyK float64
	logger        *slog.Logger
}

// NewStatic creates a scorer that maps legacy priority levels to float scores.
func NewStatic(seedBonus, normalScore, recrawlScore, depthPenaltyK float64, logger *slog.Logger) *StaticScorer {
	return &StaticScorer{
		seedBonus:     seedBonus,
		normalScore:   normalScore,
		recrawlScore:  recrawlScore,
		depthPenaltyK: depthPenaltyK,
		logger:        logger.With("component", "static_scorer"),
	}
}

func (s *StaticScorer) Name() string { return "static" }

// Score returns a fixed score based on the job's BasePriority, with a
// logarithmic depth penalty applied.
func (s *StaticScorer) Score(job *queue.Job) float64 {
	base := job.BaseScore
	if base == 0 {
		base = s.normalScore
	}

	penalty := s.depthPenaltyK * math.Log1p(float64(job.Depth))
	score := base - penalty
	if score < 0 {
		score = 0
	}
	return score
}
