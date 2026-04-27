// Package scorer provides scoring strategies for the URL frontier.
// It replaces the static integer priority system with a configurable
// scoring function that estimates the value of crawling a URL.
package scorer

import "github.com/DuvyDev/Duvycrawl/internal/queue"

// Scorer calculates a score for a queued job. Higher scores are crawled first.
type Scorer interface {
	Score(job *queue.Job) float64
	Name() string
}
