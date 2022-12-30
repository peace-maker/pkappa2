package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fsnotify/fsnotify"
	"github.com/spq/pkappa2/internal/index"
	"github.com/spq/pkappa2/internal/index/builder"
	"github.com/spq/pkappa2/internal/query"
	"github.com/spq/pkappa2/internal/tools"
	"github.com/spq/pkappa2/internal/tools/bitmask"
	pcapmetadata "github.com/spq/pkappa2/internal/tools/pcapMetadata"
)

type (
	tag struct {
		query.TagDetails
		definition string
		features   query.FeatureSet
		color      string
		converters []*index.Converter
	}
	TagInfo struct {
		Name           string
		Definition     string
		Color          string
		MatchingCount  uint
		UncertainCount uint
		Referenced     bool
	}
	Manager struct {
		StateDir     string
		PcapDir      string
		IndexDir     string
		SnapshotDir  string
		ConverterDir string

		jobs              chan func()
		mergeJobRunning   bool
		taggingJobRunning bool
		importJobs        []string

		builder             *builder.Builder
		indexes             []*index.Reader
		nStreams, nPackets  int
		nextStreamID        uint64
		nUnmergeableIndexes int
		stateFilename       string
		allStreams          bitmask.LongBitmask

		updatedStreamsDuringTaggingJob bitmask.LongBitmask
		addedStreamsDuringTaggingJob   bitmask.LongBitmask

		tags       map[string]*tag
		converters map[string]*index.Converter

		usedIndexes map[*index.Reader]uint
		watcher     *fsnotify.Watcher
	}

	Statistics struct {
		ImportJobCount            int
		IndexCount                int
		IndexLockCount            uint
		PcapCount                 int
		StreamCount               int
		PacketCount               int
		MergeJobRunning           bool
		TaggingJobRunning         bool
		ConverterProcessesRunning map[string]bool
	}

	indexReleaser []*index.Reader

	// TODO: Maybe save md5 of converters to detect changes
	stateFile struct {
		Saved time.Time
		Tags  []struct {
			Name       string
			Definition string
			Color      string
			Converters []string
		}
		Pcaps []*pcapmetadata.PcapInfo
	}

	updateTagOperationInfo struct {
		markTagAddStreams, markTagDelStreams []uint64
		color                                string
		addConverterName, delConverterName   []string
	}
	UpdateTagOperation func(*updateTagOperationInfo)

	View struct {
		mgr *Manager

		indexes  []*index.Reader
		releaser indexReleaser

		tagDetails map[string]query.TagDetails
	}

	StreamContext struct {
		s *index.Stream
		v *View
	}

	streamsOptions struct {
		prefetchTags       []string
		defaultLimit, page uint
		prefetchAllTags    bool
	}
	StreamsOption func(*streamsOptions)
)

func New(pcapDir, indexDir, snapshotDir, stateDir, converterDir string) (*Manager, error) {
	mgr := Manager{
		PcapDir:      pcapDir,
		IndexDir:     indexDir,
		SnapshotDir:  snapshotDir,
		StateDir:     stateDir,
		ConverterDir: converterDir,

		usedIndexes: make(map[*index.Reader]uint),
		tags:        make(map[string]*tag),
		converters:  make(map[string]*index.Converter),
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	mgr.startMonitoringConverters(watcher)

	// Lookup all available converter binaries
	entries, err := os.ReadDir(mgr.ConverterDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := mgr.addConverter(filepath.Join(mgr.ConverterDir, entry.Name())); err != nil {
			return nil, err
		}
	}

	tools.AssertFolderRWXPermissions("pcap_dir", pcapDir)
	tools.AssertFolderRWXPermissions("index_dir", indexDir)
	tools.AssertFolderRWXPermissions("snapshot_dir", snapshotDir)
	tools.AssertFolderRWXPermissions("state_dir", stateDir)

	// read all existing indexes and load them
	indexFileNames, err := tools.ListFiles(indexDir, "idx")
	if err != nil {
		return nil, err
	}
	for _, fn := range indexFileNames {
		idx, err := index.NewReader(fn)
		if err != nil {
			log.Printf("Unable to load index %q: %v", fn, err)
			continue
		}
		mgr.indexes = append(mgr.indexes, idx)
		mgr.nStreams += idx.StreamCount()
		mgr.nPackets += idx.PacketCount()
		if next := idx.MaxStreamID() + 1; mgr.nextStreamID < next {
			mgr.nextStreamID = next
		}
	}
	mgr.lock(mgr.indexes)

	stateFilenames, err := tools.ListFiles(stateDir, "state.json")
	if err != nil {
		return nil, err
	}
	stateTimestamp := time.Time{}
	cachedKnownPcapData := []*pcapmetadata.PcapInfo(nil)
	if mgr.nextStreamID != 0 {
		mgr.allStreams.Set(uint(mgr.nextStreamID - 1))
		for i := uint64(0); i != mgr.nextStreamID; i++ {
			mgr.allStreams.Set(uint(i))
		}
	}
nextStateFile:
	for _, fn := range stateFilenames {
		f, err := os.Open(fn)
		if err != nil {
			log.Printf("Unable to load state file %q: %v", fn, err)
			continue
		}
		s := stateFile{}
		if err := json.NewDecoder(f).Decode(&s); err != nil {
			log.Printf("Unable to parse state file %q: %v", fn, err)
			continue
		}
		if s.Saved.Before(stateTimestamp) {
			continue
		}
		newTags := make(map[string]*tag, len(s.Tags))
		for _, t := range s.Tags {
			q, err := query.Parse(t.Definition)
			if err != nil {
				log.Printf("Invalid tag %q in statefile %q: %v", t.Name, fn, err)
				continue nextStateFile
			}
			if _, ok := newTags[t.Name]; ok {
				log.Printf("Invalid tag %q in statefile %q: duplicate name", t.Name, fn)
				continue nextStateFile
			}
			nt := &tag{
				TagDetails: query.TagDetails{
					Uncertain:  mgr.allStreams,
					Conditions: q.Conditions,
				},
				definition: t.Definition,
				features:   q.Conditions.Features(),
				color:      t.Color,
			}
			if strings.HasPrefix(t.Name, "mark/") || strings.HasPrefix(t.Name, "generated/") {
				ids, ok := q.Conditions.StreamIDs(mgr.nextStreamID)
				if !ok {
					log.Printf("Invalid tag %q in statefile %q: 'mark' or 'generated' tag is malformed", t.Name, fn)
					continue nextStateFile
				}
				nt.Matches = ids
				nt.Uncertain = bitmask.LongBitmask{}
			}
			for _, converterName := range t.Converters {
				converter, ok := mgr.converters[converterName]
				if !ok {
					// TODO: just remove the cache file if any?
					log.Printf("Invalid tag %q in statefile %q: references non-existing converter %q", t.Name, fn, converterName)
					continue nextStateFile
				}
				mgr.attachConverterToTag(nt, t.Name, converter)
			}
			newTags[t.Name] = nt
		}
		cyclingTags := map[string]struct{}{}
		for n, t := range newTags {
			for _, tn := range t.referencedTags() {
				if n == tn {
					log.Printf("Invalid tag %q in statefile %q: references itself", n, fn)
					continue nextStateFile
				}
				if _, ok := newTags[tn]; !ok {
					log.Printf("Invalid tag %q in statefile %q: references non-existing tag %q", n, fn, tn)
					continue nextStateFile
				}
			}
			cyclingTags[n] = struct{}{}
		}
	checkCyclingTags:
		for {
		nextCyclingTag:
			for n := range cyclingTags {
				for _, rt := range newTags[n].referencedTags() {
					if _, ok := cyclingTags[rt]; ok {
						continue nextCyclingTag
					}
				}
				delete(cyclingTags, n)
				continue checkCyclingTags
			}
			for n := range cyclingTags {
				log.Printf("Invalid tag %q in statefile %q: contains cycle", n, fn)
				continue nextStateFile
			}
			break
		}
		mgr.tags = newTags
		mgr.stateFilename = fn
		stateTimestamp = s.Saved
		cachedKnownPcapData = s.Pcaps
	}

	mgr.builder, err = builder.New(pcapDir, indexDir, snapshotDir, cachedKnownPcapData)
	if err != nil {
		return nil, err
	}
	if len(mgr.builder.KnownPcaps()) != len(cachedKnownPcapData) {
		//nolint:errcheck
		mgr.saveState()
	}

	mgr.jobs = make(chan func())
	go func() {
		for f := range mgr.jobs {
			f()
		}
	}()
	mgr.jobs <- func() {
		mgr.startTaggingJobIfNeeded()
		mgr.startMergeJobIfNeeded()
		for tn, t := range mgr.tags {
			mgr.delegateTagMatchesToConverters(t, tn)
		}
	}
	return &mgr, nil
}

func (t tag) referencedTags() []string {
	return append(append([]string(nil), t.features.MainTags...), t.features.SubQueryTags...)
}

func (t tag) converterNames() []string {
	converterNames := make([]string, len(t.converters))
	for _, converter := range t.converters {
		converterNames = append(converterNames, converter.Name())
	}
	return converterNames
}

func (mgr *Manager) Close() {
	if mgr.watcher != nil {
		mgr.watcher.Close()
	}
}

func (mgr *Manager) saveState() error {
	j := stateFile{
		Saved: time.Now(),
		Pcaps: mgr.builder.KnownPcaps(),
	}
	for n, t := range mgr.tags {
		j.Tags = append(j.Tags, struct {
			Name       string
			Definition string
			Color      string
			Converters []string
		}{
			Name:       n,
			Definition: t.definition,
			Color:      t.color,
			Converters: t.converterNames(),
		})
	}
	fn := tools.MakeFilename(mgr.StateDir, "state.json")
	f, err := os.Create(fn)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(&j); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if mgr.stateFilename != "" {
		if err := os.Remove(mgr.stateFilename); err != nil {
			log.Printf("Unable to delete old statefile %q: %v", mgr.stateFilename, err)
		}
	}
	mgr.stateFilename = fn
	return nil
}

func (mgr *Manager) inheritTagUncertainty() {
	resolvedTags := map[string]struct{}{}
	for len(resolvedTags) != len(mgr.tags) {
	outer:
		for tn, ti := range mgr.tags {
			if _, ok := resolvedTags[tn]; ok {
				continue
			}
			for _, rtn := range ti.referencedTags() {
				if _, ok := resolvedTags[rtn]; !ok {
					continue outer
				}
			}
			resolvedTags[tn] = struct{}{}
			if len(ti.features.MainTags) == 0 && len(ti.features.SubQueryTags) == 0 {
				continue
			}
			fullyInvalidated := false
			for _, rtn := range ti.features.SubQueryTags {
				if !mgr.tags[rtn].Uncertain.IsZero() {
					//TODO: is a matching stream really uncertain?
					ti.Uncertain = mgr.allStreams
					fullyInvalidated = true
					break
				}
			}
			if !fullyInvalidated {
				ti.Uncertain = ti.Uncertain.Copy()
				for _, rtn := range ti.features.MainTags {
					ti.Uncertain.Or(mgr.tags[rtn].Uncertain)
				}
			}
			mgr.tags[tn] = ti
		}
	}
}

func (mgr *Manager) invalidateTags(updatedStreams, addedStreams bitmask.LongBitmask) {
	for tn, ti := range mgr.tags {
		tin := *ti
		if ti.features.SubQueryFeatures != 0 {
			//TODO: is a matching stream really uncertain?
			tin.Uncertain = mgr.allStreams
		} else if ti.features.MainFeatures&^query.FeatureFilterID == 0 {
			continue
		} else {
			tin.Uncertain = ti.Uncertain.Copy()
			tin.Uncertain.Or(addedStreams)
			if ti.features.MainFeatures&(query.FeatureFilterData|query.FeatureFilterTimeAbsolute|query.FeatureFilterTimeRelative) != 0 {
				tin.Uncertain.Or(updatedStreams)
			}
		}
		mgr.tags[tn] = &tin
	}
	mgr.inheritTagUncertainty()
}

func (mgr *Manager) importPcapJob(filenames []string, nextStreamID uint64, existingIndexes []*index.Reader, existingIndexesReleaser indexReleaser) {
	processedFiles, createdIndexes, err := mgr.builder.FromPcap(mgr.PcapDir, filenames, existingIndexes)
	if err != nil {
		log.Printf("importPcapJob(%q) failed: %s", filenames, err)
	}
	updatedStreams := bitmask.LongBitmask{}
	addedStreams := bitmask.LongBitmask{}
	newStreamCount, newPacketCount := 0, 0
	newNextStreamID := nextStreamID
	for _, idx := range createdIndexes {
		newStreamCount += idx.StreamCount()
		newPacketCount += idx.PacketCount()
		if next := idx.MaxStreamID() + 1; newNextStreamID < next {
			newNextStreamID = next
		}
		for i := range idx.StreamIDs() {
			if i < nextStreamID {
				updatedStreams.Set(uint(i))
			} else {
				addedStreams.Set(uint(i))
			}
		}
	}
	allStreams := bitmask.LongBitmask{}
	if newNextStreamID != 0 {
		allStreams.Set(uint(newNextStreamID - 1))
		for i := uint64(0); i < newNextStreamID; i++ {
			allStreams.Set(uint(i))
		}
	}
	mgr.jobs <- func() {
		mgr.allStreams = allStreams
		existingIndexesReleaser.release(mgr)
		// add new indexes if some were created
		if len(createdIndexes) > 0 {
			mgr.indexes = append(mgr.indexes, createdIndexes...)
			mgr.nStreams += newStreamCount
			mgr.nPackets += newPacketCount
			mgr.nextStreamID = newNextStreamID
			mgr.lock(createdIndexes)
			mgr.addedStreamsDuringTaggingJob.Or(addedStreams)
			mgr.updatedStreamsDuringTaggingJob.Or(updatedStreams)
			mgr.invalidateTags(updatedStreams, addedStreams)
		}
		// remove finished job from queue
		mgr.importJobs = mgr.importJobs[processedFiles:]
		// start new import job if there are more queued
		if len(mgr.importJobs) >= 1 {
			idxs, rel := mgr.getIndexesCopy(0)
			go mgr.importPcapJob(mgr.importJobs[:], mgr.nextStreamID, idxs, rel)
		}
		mgr.startTaggingJobIfNeeded()
		mgr.startMergeJobIfNeeded()
		//nolint:errcheck
		mgr.saveState()
	}
}

func (mgr *Manager) startMergeJobIfNeeded() {
	if mgr.mergeJobRunning || mgr.taggingJobRunning {
		return
	}
	// only merge if all tags are on the newest version, prioritize updating tags
	for _, t := range mgr.tags {
		if !t.Uncertain.IsZero() {
			return
		}
	}
	nStreams := mgr.nStreams
	for i, idx := range mgr.indexes {
		c := idx.StreamCount()
		nStreams -= c
		if i >= mgr.nUnmergeableIndexes && c < nStreams {
			mgr.mergeJobRunning = true
			indexes, indexesReleaser := mgr.getIndexesCopy(i)
			go mgr.mergeIndexesJob(i, indexes, indexesReleaser)
			return
		}
	}
}

func (mgr *Manager) startTaggingJobIfNeeded() {
	if mgr.taggingJobRunning {
		return
	}
outer:
	for n, t := range mgr.tags {
		if t.Uncertain.IsZero() {
			continue
		}
		for _, tn := range t.referencedTags() {
			if !mgr.tags[tn].Uncertain.IsZero() {
				continue outer
			}
		}
		tagDetails := make(map[string]query.TagDetails)
		for _, tn := range t.referencedTags() {
			tagDetails[tn] = mgr.tags[tn].TagDetails
		}
		mgr.updatedStreamsDuringTaggingJob = bitmask.LongBitmask{}
		mgr.addedStreamsDuringTaggingJob = bitmask.LongBitmask{}
		mgr.taggingJobRunning = true
		indexes, releaser := mgr.getIndexesCopy(0)
		go mgr.updateTagJob(n, *t, tagDetails, indexes, releaser)
		return
	}
}

func (mgr *Manager) mergeIndexesJob(offset int, indexes []*index.Reader, releaser indexReleaser) {
	mergedIndexes, err := index.Merge(mgr.IndexDir, indexes)
	if err != nil {
		indexFilenames := []string{}
		for _, i := range indexes {
			indexFilenames = append(indexFilenames, i.Filename())
		}
		log.Printf("mergeIndexesJob(%d, [%q]) failed: %s", offset, indexFilenames, err)
	}
	streamsDiff, packetsDiff := 0, 0
	for _, idx := range mergedIndexes {
		streamsDiff += idx.StreamCount()
		packetsDiff += idx.PacketCount()
	}
	for _, idx := range indexes {
		streamsDiff -= idx.StreamCount()
		packetsDiff -= idx.PacketCount()
	}
	mgr.jobs <- func() {
		// replace old indexes if successfully created
		if len(mergedIndexes) == 0 || err != nil {
			mgr.nUnmergeableIndexes++
		} else {
			rel := indexReleaser(mgr.indexes[offset : offset+len(indexes)])
			rel.release(mgr)
			mgr.lock(mergedIndexes)
			mgr.indexes = append(mgr.indexes[:offset], append(mergedIndexes, mgr.indexes[offset+len(indexes):]...)...)
			mgr.nUnmergeableIndexes += len(mergedIndexes) - 1
			mgr.nStreams += streamsDiff
			mgr.nPackets += packetsDiff
		}
		mgr.mergeJobRunning = false
		mgr.startMergeJobIfNeeded()
		releaser.release(mgr)
	}
}

func (mgr *Manager) updateTagJob(name string, t tag, tagDetails map[string]query.TagDetails, indexes []*index.Reader, releaser indexReleaser) {
	err := func() error {
		q, err := query.Parse(t.definition)
		if err != nil {
			return err
		}
		streams, _, err := index.SearchStreams(indexes, mgr.converters, &t.Uncertain, q.ReferenceTime, q.Conditions, nil, []query.Sorting{{Key: query.SortingKeyID, Dir: query.SortingDirAscending}}, 0, 0, tagDetails)
		if err != nil {
			return err
		}
		t.Matches = t.Matches.Copy()
		t.Matches.Sub(t.Uncertain)
		for _, s := range streams {
			t.Matches.Set(uint(s.ID()))
			// TODO: pass in the streams slice at once instead of every single stream individually.
			// run converters for this stream if any are attached to this tag
			// don't run converters multiple times if multiple matching tags are attached to the converter
			for _, converter := range t.converters {
				if !converter.HasStream(s.ID()) {
					converter.EnqueueStream(s)
				}
			}
		}
		return nil
	}()
	if err != nil {
		log.Printf("updateTagJob failed: %q", err)
		t.Matches = bitmask.LongBitmask{}
	}
	t.Uncertain = bitmask.LongBitmask{}
	mgr.jobs <- func() {
		// don't touch the tag if it was modified
		if ot, ok := mgr.tags[name]; ok && ot.definition == t.definition {
			mgr.tags[name] = &t
			if !(mgr.updatedStreamsDuringTaggingJob.IsZero() && mgr.addedStreamsDuringTaggingJob.IsZero()) {
				mgr.invalidateTags(mgr.updatedStreamsDuringTaggingJob, mgr.addedStreamsDuringTaggingJob)
			}
		}
		mgr.taggingJobRunning = false
		mgr.startTaggingJobIfNeeded()
		mgr.startMergeJobIfNeeded()
		releaser.release(mgr)
	}
}

func (mgr *Manager) ImportPcap(filename string) {
	mgr.jobs <- func() {
		//add job to be processed by importer goroutine
		mgr.importJobs = append(mgr.importJobs, filename)
		//start import job when none running
		if len(mgr.importJobs) == 1 {
			indexes, releaser := mgr.getIndexesCopy(0)
			go mgr.importPcapJob(mgr.importJobs[:1], mgr.nextStreamID, indexes, releaser)
		}
	}
}

func (mgr *Manager) getIndexesCopy(start int) ([]*index.Reader, indexReleaser) {
	indexes := append([]*index.Reader(nil), mgr.indexes[start:]...)
	return indexes, mgr.lock(indexes)
}

func (mgr *Manager) Status() Statistics {
	c := make(chan Statistics)
	mgr.jobs <- func() {
		locks := uint(0)
		for _, n := range mgr.usedIndexes {
			locks += n
		}
		converterProcessesRunning := make(map[string]bool)
		for name, converter := range mgr.converters {
			converterProcessesRunning[name] = converter.IsRunning()
		}
		c <- Statistics{
			IndexCount:                len(mgr.indexes),
			IndexLockCount:            locks,
			PcapCount:                 len(mgr.builder.KnownPcaps()),
			ImportJobCount:            len(mgr.importJobs),
			StreamCount:               mgr.nStreams,
			PacketCount:               mgr.nPackets,
			MergeJobRunning:           mgr.mergeJobRunning,
			TaggingJobRunning:         mgr.taggingJobRunning,
			ConverterProcessesRunning: converterProcessesRunning,
		}
		close(c)
	}
	res := <-c
	return res
}

func (mgr *Manager) KnownPcaps() []pcapmetadata.PcapInfo {
	c := make(chan []pcapmetadata.PcapInfo)
	mgr.jobs <- func() {
		r := []pcapmetadata.PcapInfo{}
		for _, p := range mgr.builder.KnownPcaps() {
			r = append(r, *p)
		}
		c <- r
		close(c)
	}
	res := <-c
	return res
}

func (mgr *Manager) ListConverters() []string {
	c := make(chan []string)
	mgr.jobs <- func() {
		r := []string{}
		for _, converter := range mgr.converters {
			r = append(r, converter.Name())
		}
		c <- r
		close(c)
	}
	res := <-c
	return res
}

func (mgr *Manager) ListTags() []TagInfo {
	c := make(chan []TagInfo)
	mgr.jobs <- func() {
		res := []TagInfo{}
		referencedTag := map[string]struct{}{}
		for _, t := range mgr.tags {
			for _, tn := range t.referencedTags() {
				referencedTag[tn] = struct{}{}
			}
		}
		for name, t := range mgr.tags {
			m := t.Matches.Copy()
			m.Sub(t.Uncertain)
			_, referenced := referencedTag[name]
			res = append(res, TagInfo{
				Name:           name,
				Definition:     t.definition,
				Color:          t.color,
				MatchingCount:  uint(m.OnesCount()),
				UncertainCount: uint(t.Uncertain.OnesCount()),
				Referenced:     referenced,
			})
		}
		sort.Slice(res, func(i, j int) bool {
			return res[i].Name < res[j].Name
		})
		c <- res
		close(c)
	}
	return <-c
}

func (mgr *Manager) AddTag(name, color, queryString string) error {
	isMark := strings.HasPrefix(name, "mark/") || strings.HasPrefix(name, "generated/")
	if !(strings.HasPrefix(name, "tag/") || strings.HasPrefix(name, "service/") || isMark) {
		return errors.New("invalid tag name (need a 'tag/', 'service/', 'mark/' or 'generated/' prefix)")
	}
	if sub := strings.SplitN(name, "/", 2)[1]; sub == "" {
		return errors.New("invalid tag name (prefix only not allowed)")
	}
	q, err := query.Parse(queryString)
	if err != nil {
		return err
	}
	features := q.Conditions.Features()
	if (features.MainFeatures|features.SubQueryFeatures)&query.FeatureFilterTimeRelative != 0 {
		return errors.New("relative times not yet supported in tags")
	}
	if q.Grouping != nil {
		return errors.New("grouping not allowed in tags")
	}
	nt := &tag{
		TagDetails: query.TagDetails{
			Conditions: q.Conditions,
		},
		definition: queryString,
		features:   features,
		color:      color,
	}
	for _, tn := range nt.referencedTags() {
		if tn == name {
			return errors.New("self reference not allowed in tags")
		}
	}
	if isMark {
		if _, ok := q.Conditions.StreamIDs(0); !ok {
			return errors.New("tags of type `mark` have to only contain an `id` filter")
		}
	}
	c := make(chan error)
	mgr.jobs <- func() {
		err := func() error {
			if _, ok := mgr.tags[name]; ok {
				return errors.New("tag already exists")
			}
			// check if all referenced tags exist
			for _, t := range nt.referencedTags() {
				if _, ok := mgr.tags[t]; !ok {
					return fmt.Errorf("unknown referenced tag %q", t)
				}
			}
			if isMark {
				nt.Matches, _ = q.Conditions.StreamIDs(mgr.nextStreamID)
			} else {
				nt.Uncertain = mgr.allStreams
			}
			mgr.tags[name] = nt
			if !isMark {
				mgr.startTaggingJobIfNeeded()
			}
			return nil
		}()
		c <- err
		close(c)
		//nolint:errcheck
		mgr.saveState()
	}
	return <-c
}

func (mgr *Manager) DelTag(name string) error {
	c := make(chan error)
	mgr.jobs <- func() {
		err := func() error {
			tag, ok := mgr.tags[name]
			if !ok {
				return fmt.Errorf("unknown tag %q", name)
			}
			for t2name, t2 := range mgr.tags {
				for _, tn := range t2.referencedTags() {
					if tn == name {
						return fmt.Errorf("tag %q still references the tag to be deleted", t2name)
					}
				}
			}
			// remove converter results of attached converters from cache
			if len(tag.converters) > 0 {
				for _, converter := range tag.converters {
					if err := mgr.detachConverterFromTag(tag, name, converter); err != nil {
						return err
					}
				}
			}
			delete(mgr.tags, name)
			return nil
		}()
		c <- err
		close(c)
		//nolint:errcheck
		mgr.saveState()
	}
	return <-c
}

func UpdateTagOperationMarkAddStream(streams []uint64) UpdateTagOperation {
	s := make([]uint64, 0, len(streams))
	s = append(s, streams...)
	return func(i *updateTagOperationInfo) {
		i.markTagAddStreams = s
	}
}

func UpdateTagOperationMarkDelStream(streams []uint64) UpdateTagOperation {
	s := make([]uint64, 0, len(streams))
	s = append(s, streams...)
	return func(i *updateTagOperationInfo) {
		i.markTagDelStreams = s
	}
}

func UpdateTagOperationUpdateColor(color string) UpdateTagOperation {
	return func(i *updateTagOperationInfo) {
		i.color = color
	}
}

func UpdateTagOperationAddConverter(converterName []string) UpdateTagOperation {
	return func(i *updateTagOperationInfo) {
		i.addConverterName = converterName
	}
}

func UpdateTagOperationDeleteConverter(converterName []string) UpdateTagOperation {
	return func(i *updateTagOperationInfo) {
		i.delConverterName = converterName
	}
}

func (mgr *Manager) UpdateTag(name string, operation UpdateTagOperation) error {
	info := updateTagOperationInfo{}
	operation(&info)
	maxUsedStreamID := uint64(0)
	if len(info.markTagAddStreams) != 0 || len(info.markTagDelStreams) != 0 {
		if !(strings.HasPrefix(name, "mark/") || strings.HasPrefix(name, "generated/")) {
			return fmt.Errorf("tag %q is not of type 'mark' or 'enerated'", name)
		}
		for _, s := range info.markTagAddStreams {
			if maxUsedStreamID <= s {
				maxUsedStreamID = s + 1
			}
		}
		for _, s := range info.markTagDelStreams {
			if maxUsedStreamID <= s {
				maxUsedStreamID = s + 1
			}
		}
		if maxUsedStreamID == 0 {
			// no operation
			return nil
		}
		maxUsedStreamID--
	}
	c := make(chan error)
	mgr.jobs <- func() {
		err := func() error {
			t, ok := mgr.tags[name]
			if !ok {
				return fmt.Errorf("unknown tag %q", name)
			}
			if info.color != "" {
				t.color = info.color
			}
			if len(info.addConverterName) != 0 {
				for _, converterName := range info.addConverterName {
					if converter, ok := mgr.converters[converterName]; !ok {
						return fmt.Errorf("unknown converter %q", converterName)
					} else {
						mgr.attachAndDelegateConverterToTag(t, name, converter)
						mgr.saveState()
					}
				}
			}
			if len(info.delConverterName) != 0 {
				for _, converterName := range info.delConverterName {
					if converter, ok := mgr.converters[converterName]; !ok {
						return fmt.Errorf("unknown converter %q", converterName)
					} else {
						mgr.detachConverterFromTag(t, name, converter)
						mgr.saveState()
					}
				}
			}
			if maxUsedStreamID != 0 {
				if maxUsedStreamID >= mgr.nextStreamID {
					return fmt.Errorf("unknown stream id %d", maxUsedStreamID)
				}
				newTag := *t
				newTag.Matches = t.Matches.Copy()
				for _, s := range info.markTagAddStreams {
					newTag.Matches.Set(uint(s))
					newTag.Uncertain.Set(uint(s))
				}
				for _, s := range info.markTagDelStreams {
					newTag.Matches.Unset(uint(s))
					newTag.Uncertain.Set(uint(s))
				}

				b := strings.Builder{}
				if newTag.Matches.IsZero() {
					b.WriteString("id:-1")
				} else {
					b.WriteString("id:")
					last := uint(0)
					for {
						zeros := newTag.Matches.TrailingZerosFrom(last)
						if zeros < 0 {
							break
						}
						if last != 0 {
							b.WriteByte(',')
						}
						last += uint(zeros)
						b.WriteString(fmt.Sprintf("%d", last))
						last++
					}
				}
				newTag.definition = b.String()
				if q, err := query.Parse(newTag.definition); err == nil {
					newTag.Conditions = q.Conditions
				}
				mgr.tags[name] = &newTag
				mgr.inheritTagUncertainty()
				mgr.tags[name].Uncertain = bitmask.LongBitmask{}
				mgr.startTaggingJobIfNeeded()
			}
			return nil
		}()
		c <- err
		close(c)
		//nolint:errcheck
		mgr.saveState()
	}
	return <-c
}

func (mgr *Manager) lock(indexes []*index.Reader) indexReleaser {
	for _, i := range indexes {
		mgr.usedIndexes[i]++
	}
	return indexReleaser(append([]*index.Reader(nil), indexes...))
}

// release all contained indexes from within the mgr goroutine
func (r *indexReleaser) release(mgr *Manager) {
	for _, i := range *r {
		mgr.usedIndexes[i]--
		if mgr.usedIndexes[i] == 0 {
			delete(mgr.usedIndexes, i)
			i.Close()
			os.Remove(i.Filename())
		}
	}
}

func (mgr *Manager) startMonitoringConverters(watcher *fsnotify.Watcher) {
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				ch := make(chan struct{})
				mgr.jobs <- func() {
					log.Println("event:", event)
					if event.Has(fsnotify.Create) {
						mgr.addConverter(event.Name)
					}
					if event.Has(fsnotify.Remove) {
						mgr.removeConverter(event.Name)
					}
					if event.Has(fsnotify.Write) {
						mgr.restartConverterProcess(event.Name)
					}
					close(ch)
				}
				<-ch
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("error:", err)
			}
		}
	}()

	err := watcher.Add(mgr.ConverterDir)
	if err != nil {
		log.Fatal(err)
	}
}

func (mgr *Manager) addConverter(path string) error {
	err := unix.Access(path, unix.X_OK)
	if err != nil {
		return fmt.Errorf("error: converter %s is not executable", path)
	}

	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	converter, err := index.NewConverter(path, name, mgr.IndexDir)
	if err != nil {
		return err
	}
	mgr.converters[name] = converter
	return nil
}

func (mgr *Manager) removeConverter(path string) error {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	converter, ok := mgr.converters[name]
	if !ok {
		return fmt.Errorf("error: converter %s does not exist", name)
	}

	// remove converter from all tags
	for _, t := range mgr.tags {
		mgr.detachConverterFromTag(t, name, converter)
	}

	// Stop the process if it is running
	if err := converter.KillProcess(); err != nil {
		return err
	}

	delete(mgr.converters, name)
	return nil
}

func (mgr *Manager) restartConverterProcess(path string) error {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	converter, ok := mgr.converters[name]
	if !ok {
		return fmt.Errorf("error: converter %s does not exist, cannot restart", name)
	}
	// Stop the process if it is running and restart it
	if err := converter.Reset(); err != nil {
		return err
	}

	return nil
}

func (mgr *Manager) attachAndDelegateConverterToTag(tag *tag, tagName string, converter *index.Converter) {
	if mgr.attachConverterToTag(tag, tagName, converter) {
		mgr.delegateTagMatchesToConverters(tag, tagName)
	}
}

func (mgr *Manager) attachConverterToTag(tag *tag, tagName string, converter *index.Converter) bool {
	// check if converter already exists
	for _, c := range tag.converters {
		if c == converter {
			return false
		}
	}

	// FIXME assert low complexity of this tag's query

	tag.converters = append(tag.converters, converter)
	converter.AttachTag(tagName)
	return true
}

func (mgr *Manager) delegateTagMatchesToConverters(tag *tag, tagName string) {
	for _, converter := range tag.converters {
		var streamIDs []uint64
		streamID := uint(0)
		for {
			zeros := tag.Matches.TrailingZerosFrom(streamID)
			if zeros < 0 {
				break
			}
			streamID += uint(zeros)
			streamIDs = append(streamIDs, uint64(streamID))
			streamID++
		}
		go mgr.DelegateStreamsToConverter(converter, streamIDs)
	}
}

func (mgr *Manager) DelegateStreamsToConverter(converter *index.Converter, streamIDs []uint64) {
	view := mgr.GetView()
	defer view.Release()

	// TODO: iterate over all indices and create a slice of all stream objects matching the tag
	for _, streamID := range streamIDs {
		stream, err := view.Stream(uint64(streamID))
		if err != nil {
			log.Printf("Converter (%s): could not get stream %d: %q", converter.Name(), streamID, err)
			continue
		}
		// FIXME: this panics if the converter is closed while we are sending
		if !converter.HasStream(streamID) {
			converter.EnqueueStream(stream.Stream())
		}
	}
}

func (mgr *Manager) detachConverterFromTag(tag *tag, tagName string, converter *index.Converter) error {
	for i, c := range tag.converters {
		if c == converter {
			tag.converters = append(tag.converters[:i], tag.converters[i+1:]...)
			break
		}
	}
	if err := converter.DetachTag(tagName); err != nil {
		return fmt.Errorf("error: could not detach converter %s from tag %s: %w", converter.Name(), tagName, err)
	}

	// TODO: delete/invalidate converter results for all matching streams now
	return nil
}

func (mgr *Manager) GetView() View {
	return View{mgr: mgr}
}

func (v *View) fetch() error {
	if len(v.indexes) != 0 {
		return nil
	}
	v.tagDetails = make(map[string]query.TagDetails)
	c := make(chan error)
	v.mgr.jobs <- func() {
		v.indexes, v.releaser = v.mgr.getIndexesCopy(0)
		for tn, ti := range v.mgr.tags {
			v.tagDetails[tn] = ti.TagDetails
		}
		c <- nil
		close(c)
	}
	return <-c
}

func (v *View) Release() {
	if len(v.releaser) != 0 {
		v.mgr.jobs <- func() {
			v.releaser.release(v.mgr)
		}
	}
}

func PrefetchTags(tags []string) StreamsOption {
	return func(o *streamsOptions) {
		o.prefetchTags = append(o.prefetchTags, tags...)
	}
}

func PrefetchAllTags() StreamsOption {
	return func(o *streamsOptions) {
		o.prefetchAllTags = true
	}
}

func Limit(defaultLimit, page uint) StreamsOption {
	return func(o *streamsOptions) {
		o.defaultLimit = defaultLimit
		o.page = page
	}
}

func (v *View) prefetchTags(tags []string, bm bitmask.LongBitmask) error {
	if len(tags) == 0 {
		return nil
	}
	uncertainTags := map[string]bitmask.LongBitmask{}
	addTag := (func(string, bitmask.LongBitmask))(nil)
	addTag = func(tn string, streams bitmask.LongBitmask) {
		ti := v.tagDetails[tn]
		if ti.Uncertain.IsZero() {
			return
		}
		uncertain := ti.Uncertain
		if !streams.IsZero() {
			uncertain = uncertain.Copy()
			uncertain.And(streams)
			if uncertain.IsZero() {
				return
			}
		}
		if u, ok := uncertainTags[tn]; ok {
			tmp := uncertain.Copy()
			tmp.Sub(u)
			if tmp.IsZero() {
				return
			}
			tmp.Or(u)
			uncertain = tmp
		}
		uncertainTags[tn] = uncertain
		f := ti.Conditions.Features()
		for _, tn := range f.SubQueryTags {
			addTag(tn, bitmask.LongBitmask{})
		}
		for _, tn := range f.MainTags {
			addTag(tn, uncertain)
		}
	}
	for _, tn := range tags {
		if _, ok := v.tagDetails[tn]; !ok {
			return fmt.Errorf("tag %q not defined", tn)
		}
		addTag(tn, bm)
	}
	for len(uncertainTags) != 0 {
	outer:
		for tn, uncertain := range uncertainTags {
			ti := v.tagDetails[tn]
			f := ti.Conditions.Features()
			for _, rtn := range f.MainTags {
				if _, ok := uncertainTags[rtn]; ok {
					continue outer
				}
			}
			for _, rtn := range f.SubQueryTags {
				if _, ok := uncertainTags[rtn]; ok {
					continue outer
				}
			}
			matches, _, err := index.SearchStreams(v.indexes, v.mgr.converters, &uncertain, time.Time{}, ti.Conditions, nil, []query.Sorting{{Key: query.SortingKeyID, Dir: query.SortingDirAscending}}, 0, 0, v.tagDetails)
			if err != nil {
				return err
			}
			ti.Uncertain = ti.Uncertain.Copy()
			ti.Uncertain.Sub(uncertain)
			ti.Matches = ti.Matches.Copy()
			ti.Matches.Sub(uncertain)
			for _, s := range matches {
				ti.Matches.Set(uint(s.StreamID))
			}
			v.tagDetails[tn] = ti
			delete(uncertainTags, tn)
		}
	}
	return nil
}

func (v *View) AllStreams(f func(StreamContext) error, options ...StreamsOption) error {
	opts := streamsOptions{}
	for _, o := range options {
		o(&opts)
	}
	if opts.defaultLimit != 0 || opts.page != 0 {
		return errors.New("Limit not supported for AllStreams")
	}
	if err := v.fetch(); err != nil {
		return err
	}
	if opts.prefetchAllTags {
		for tn := range v.tagDetails {
			opts.prefetchTags = append(opts.prefetchTags, tn)
		}
	}
	v.prefetchTags(opts.prefetchTags, bitmask.LongBitmask{})
	for i := len(v.indexes); i > 0; i-- {
		idx := v.indexes[i-1]
		if err := idx.AllStreams(func(s *index.Stream) error {
			for _, idx2 := range v.indexes[i:] {
				if _, ok := idx2.StreamIDs()[s.ID()]; ok {
					return nil
				}
			}
			return f(StreamContext{
				s: s,
				v: v,
			})
		}); err != nil {
			return err
		}
	}
	return nil
}

func (v *View) SearchStreams(filter *query.Query, f func(StreamContext) error, options ...StreamsOption) (bool, uint, error) {
	opts := streamsOptions{}
	for _, o := range options {
		o(&opts)
	}
	if err := v.fetch(); err != nil {
		return false, 0, err
	}
	if opts.prefetchAllTags {
		for tn := range v.tagDetails {
			opts.prefetchTags = append(opts.prefetchTags, tn)
		}
	}
	limit := opts.defaultLimit
	if filter.Limit != nil {
		limit = *filter.Limit
	}
	offset := opts.page * limit
	res, hasMore, err := index.SearchStreams(v.indexes, v.mgr.converters, nil, filter.ReferenceTime, filter.Conditions, filter.Grouping, filter.Sorting, limit, offset, v.tagDetails)
	if err != nil {
		return false, 0, err
	}
	if len(res) == 0 {
		return hasMore, offset, nil
	}
	if len(opts.prefetchTags) != 0 {
		searchedStreams := bitmask.LongBitmask{}
		for _, s := range res {
			searchedStreams.Set(uint(s.StreamID))
		}
		if err := v.prefetchTags(opts.prefetchTags, searchedStreams); err != nil {
			return false, 0, err
		}
	}
	for _, s := range res {
		if err := f(StreamContext{
			s: s,
			v: v,
		}); err != nil {
			return false, 0, err
		}
	}
	return hasMore, offset, nil
}

func (v *View) ReferenceTime() (time.Time, error) {
	if err := v.fetch(); err != nil {
		return time.Time{}, err
	}
	referenceTime := time.Time{}
	for _, idx := range v.indexes {
		if referenceTime.IsZero() || referenceTime.After(idx.ReferenceTime) {
			referenceTime = idx.ReferenceTime
		}
	}
	return referenceTime, nil
}

func (v *View) Stream(streamID uint64) (StreamContext, error) {
	if err := v.fetch(); err != nil {
		return StreamContext{}, err
	}
	for i := len(v.indexes) - 1; i >= 0; i-- {
		idx := v.indexes[i]
		stream, err := idx.StreamByID(streamID)
		if err != nil {
			return StreamContext{}, err
		}
		if stream == nil {
			continue
		}
		return StreamContext{
			s: stream,
			v: v,
		}, nil
	}
	return StreamContext{}, nil
}

func (c StreamContext) Stream() *index.Stream {
	return c.s
}

func (c StreamContext) Data(converterName string) ([]index.Data, error) {
	if c.Stream() == nil {
		return nil, fmt.Errorf("stream not found")
	}
	if converterName == "" {
		return c.Stream().Data()
	}
	for tn := range c.v.tagDetails {
		ok, err := c.HasTag(tn)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		tag := c.v.mgr.tags[tn]
		for _, converter := range tag.converters {
			if converter.Name() == converterName {
				data, _, _, err := converter.Data(c.Stream().StreamID)
				return data, err
			}
		}
	}
	return nil, fmt.Errorf("converter %q not attached to a tag of the stream", converterName)
}

func (c StreamContext) HasTag(name string) (bool, error) {
	td := c.v.tagDetails[name]
	if !td.Uncertain.IsSet(uint(c.s.ID())) {
		return td.Matches.IsSet(uint(c.s.ID())), nil
	}
	//TODO: figure out if the uncertain tag matches
	return false, nil
}

func (c StreamContext) AllTags() ([]string, error) {
	tags := []string{}
	for tn, td := range c.v.tagDetails {
		if !td.Uncertain.IsSet(uint(c.s.ID())) {
			if td.Matches.IsSet(uint(c.s.ID())) {
				tags = append(tags, tn)
			}
			continue
		}
		//TODO: figure out if the uncertain tag matches
	}
	sort.Strings(tags)
	return tags, nil
}

func (c StreamContext) AllConverters() ([]string, error) {
	converters := []string{}
	for tn := range c.v.tagDetails {
		ok, err := c.HasTag(tn)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		tag := c.v.mgr.tags[tn]
		for _, converter := range tag.converters {
			converters = append(converters, converter.Name())
		}
	}
	sort.Strings(converters)
	return converters, nil
}
