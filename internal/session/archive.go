package session

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// RunArchive scans closed sessions past retention days, moves them to archive/,
// then enforces the archive count limit by deleting the oldest archives.
//
// Failures are logged as warnings and skipped — archive errors never break the system.
func (sm *SessionManager) RunArchive() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	archiveDir := filepath.Join(sm.baseDir, "archive")

	// Step 1: Scan for closed sessions past retention
	pattern := filepath.Join(sm.baseDir, "sess-*", "metadata.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob sessions: %w", err)
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -sm.cfg.RetentionDays)

	for _, metaPath := range matches {
		sessDir := filepath.Dir(metaPath)

		meta, err := LoadMetadata(metaPath)
		if err != nil {
			log.Printf("[archive] WARN: skip %s: load metadata: %v", sessDir, err)
			continue
		}

		// Only archive closed sessions
		if meta.Status != "closed" {
			continue
		}

		// Check if past retention
		createdAt, err := time.Parse(time.RFC3339Nano, meta.CreatedAt)
		if err != nil {
			// Try RFC3339 as fallback
			createdAt, err = time.Parse(time.RFC3339, meta.CreatedAt)
			if err != nil {
				log.Printf("[archive] WARN: skip %s: parse created_at: %v", sessDir, err)
				continue
			}
		}

		if createdAt.After(cutoff) {
			continue // Not yet past retention
		}

		// Move to archive
		if err := os.MkdirAll(archiveDir, 0755); err != nil {
			log.Printf("[archive] WARN: create archive dir: %v", err)
			continue
		}

		destDir := filepath.Join(archiveDir, filepath.Base(sessDir))
		if err := os.Rename(sessDir, destDir); err != nil {
			log.Printf("[archive] WARN: move %s to archive: %v", sessDir, err)
			continue
		}
	}

	// Step 2: Enforce archive count limit
	if err := sm.cleanupArchives(archiveDir); err != nil {
		log.Printf("[archive] WARN: cleanup archives: %v", err)
	}

	return nil
}

// cleanupArchives deletes the oldest archives when count exceeds archive_max.
func (sm *SessionManager) cleanupArchives(archiveDir string) error {
	if sm.cfg.ArchiveMax <= 0 {
		return nil // No limit
	}

	pattern := filepath.Join(archiveDir, "sess-*", "metadata.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob archives: %w", err)
	}

	if len(matches) <= sm.cfg.ArchiveMax {
		return nil // Within limit
	}

	// Load metadata for sorting
	type archivedSession struct {
		dir       string
		createdAt time.Time
	}

	var sessions []archivedSession
	for _, metaPath := range matches {
		meta, err := LoadMetadata(metaPath)
		if err != nil {
			log.Printf("[archive] WARN: skip cleanup %s: %v", metaPath, err)
			continue
		}
		createdAt, err := time.Parse(time.RFC3339Nano, meta.CreatedAt)
		if err != nil {
			createdAt, err = time.Parse(time.RFC3339, meta.CreatedAt)
			if err != nil {
				log.Printf("[archive] WARN: skip cleanup %s: parse time: %v", metaPath, err)
				continue
			}
		}
		sessions = append(sessions, archivedSession{
			dir:       filepath.Dir(metaPath),
			createdAt: createdAt,
		})
	}

	if len(sessions) <= sm.cfg.ArchiveMax {
		return nil
	}

	// Sort by created_at ascending (oldest first)
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].createdAt.Before(sessions[j].createdAt)
	})

	// Delete oldest until within limit
	toDelete := len(sessions) - sm.cfg.ArchiveMax
	for i := 0; i < toDelete; i++ {
		if err := os.RemoveAll(sessions[i].dir); err != nil {
			log.Printf("[archive] WARN: delete archive %s: %v", sessions[i].dir, err)
			continue
		}
	}

	return nil
}
