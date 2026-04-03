package model

import "time"

type Claim struct {
	AgentID   string
	FilePath  string
	ClaimedAt time.Time
}
