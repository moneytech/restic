package main

import (
	"fmt"
	"restic"
	"restic/debug"
	"restic/errors"
	"restic/index"
	"restic/repository"
	"time"

	"github.com/spf13/cobra"
)

var cmdPrune = &cobra.Command{
	Use:   "prune [flags]",
	Short: "remove unneeded data from the repository",
	Long: `
The "prune" command checks the repository and removes data that is not
referenced and therefore not needed any more.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPrune(globalOptions)
	},
}

func init() {
	cmdRoot.AddCommand(cmdPrune)
}

// newProgressMax returns a progress that counts blobs.
func newProgressMax(show bool, max uint64, description string) *restic.Progress {
	if !show {
		return nil
	}

	p := restic.NewProgress()

	p.OnUpdate = func(s restic.Stat, d time.Duration, ticker bool) {
		status := fmt.Sprintf("[%s] %s  %d / %d %s",
			formatDuration(d),
			formatPercent(s.Blobs, max),
			s.Blobs, max, description)

		if w := stdoutTerminalWidth(); w > 0 {
			if len(status) > w {
				max := w - len(status) - 4
				status = status[:max] + "... "
			}
		}

		PrintProgress("%s", status)
	}

	p.OnDone = func(s restic.Stat, d time.Duration, ticker bool) {
		fmt.Printf("\n")
	}

	return p
}

func runPrune(gopts GlobalOptions) error {
	repo, err := OpenRepository(gopts)
	if err != nil {
		return err
	}

	lock, err := lockRepoExclusive(repo)
	defer unlockRepo(lock)
	if err != nil {
		return err
	}

	return pruneRepository(gopts, repo)
}

func pruneRepository(gopts GlobalOptions, repo restic.Repository) error {
	ctx := gopts.ctx

	err := repo.LoadIndex(ctx)
	if err != nil {
		return err
	}

	var stats struct {
		blobs     int
		packs     int
		snapshots int
		bytes     int64
	}

	Verbosef("counting files in repo\n")
	for range repo.List(ctx, restic.DataFile) {
		stats.packs++
	}

	Verbosef("building new index for repo\n")

	bar := newProgressMax(!gopts.Quiet, uint64(stats.packs), "packs")
	idx, err := index.New(ctx, repo, bar)
	if err != nil {
		return err
	}

	blobs := 0
	for _, pack := range idx.Packs {
		stats.bytes += pack.Size
		blobs += len(pack.Entries)
	}
	Verbosef("repository contains %v packs (%v blobs) with %v bytes\n",
		len(idx.Packs), blobs, formatBytes(uint64(stats.bytes)))

	blobCount := make(map[restic.BlobHandle]int)
	duplicateBlobs := 0
	duplicateBytes := 0

	// find duplicate blobs
	for _, p := range idx.Packs {
		for _, entry := range p.Entries {
			stats.blobs++
			h := restic.BlobHandle{ID: entry.ID, Type: entry.Type}
			blobCount[h]++

			if blobCount[h] > 1 {
				duplicateBlobs++
				duplicateBytes += int(entry.Length)
			}
		}
	}

	Verbosef("processed %d blobs: %d duplicate blobs, %v duplicate\n",
		stats.blobs, duplicateBlobs, formatBytes(uint64(duplicateBytes)))
	Verbosef("load all snapshots\n")

	// find referenced blobs
	snapshots, err := restic.LoadAllSnapshots(ctx, repo)
	if err != nil {
		return err
	}

	stats.snapshots = len(snapshots)

	Verbosef("find data that is still in use for %d snapshots\n", stats.snapshots)

	usedBlobs := restic.NewBlobSet()
	seenBlobs := restic.NewBlobSet()

	bar = newProgressMax(!gopts.Quiet, uint64(len(snapshots)), "snapshots")
	bar.Start()
	for _, sn := range snapshots {
		debug.Log("process snapshot %v", sn.ID().Str())

		err = restic.FindUsedBlobs(ctx, repo, *sn.Tree, usedBlobs, seenBlobs)
		if err != nil {
			return err
		}

		debug.Log("found %v blobs for snapshot %v", sn.ID().Str())
		bar.Report(restic.Stat{Blobs: 1})
	}
	bar.Done()

	Verbosef("found %d of %d data blobs still in use, removing %d blobs\n",
		len(usedBlobs), stats.blobs, stats.blobs-len(usedBlobs))

	// find packs that need a rewrite
	rewritePacks := restic.NewIDSet()
	for _, pack := range idx.Packs {
		for _, blob := range pack.Entries {
			h := restic.BlobHandle{ID: blob.ID, Type: blob.Type}
			if !usedBlobs.Has(h) {
				rewritePacks.Insert(pack.ID)
				continue
			}

			if blobCount[h] > 1 {
				rewritePacks.Insert(pack.ID)
			}
		}
	}

	removeBytes := duplicateBytes

	// find packs that are unneeded
	removePacks := restic.NewIDSet()
	for packID, p := range idx.Packs {

		hasActiveBlob := false
		for _, blob := range p.Entries {
			h := restic.BlobHandle{ID: blob.ID, Type: blob.Type}
			if usedBlobs.Has(h) {
				hasActiveBlob = true
				continue
			}

			removeBytes += int(blob.Length)
		}

		if hasActiveBlob {
			continue
		}

		removePacks.Insert(packID)

		if !rewritePacks.Has(packID) {
			return errors.Fatalf("pack %v is unneeded, but not contained in rewritePacks", packID.Str())
		}

		rewritePacks.Delete(packID)
	}

	Verbosef("will delete %d packs and rewrite %d packs, this frees %s\n",
		len(removePacks), len(rewritePacks), formatBytes(uint64(removeBytes)))

	if len(rewritePacks) != 0 {
		bar = newProgressMax(!gopts.Quiet, uint64(len(rewritePacks)), "packs rewritten")
		bar.Start()
		err = repository.Repack(ctx, repo, rewritePacks, usedBlobs, bar)
		if err != nil {
			return err
		}
		bar.Done()
	}

	if len(removePacks) != 0 {
		bar = newProgressMax(!gopts.Quiet, uint64(len(removePacks)), "packs deleted")
		bar.Start()
		for packID := range removePacks {
			h := restic.Handle{Type: restic.DataFile, Name: packID.String()}
			err = repo.Backend().Remove(ctx, h)
			if err != nil {
				Warnf("unable to remove file %v from the repository\n", packID.Str())
			}
			bar.Report(restic.Stat{Blobs: 1})
		}
		bar.Done()
	}

	if err = rebuildIndex(ctx, repo); err != nil {
		return err
	}

	Verbosef("done\n")
	return nil
}
