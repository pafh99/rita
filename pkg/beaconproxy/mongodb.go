package beaconproxy

import (
	"runtime"

	"github.com/activecm/rita/config"
	"github.com/activecm/rita/database"
	"github.com/activecm/rita/pkg/data"
	"github.com/activecm/rita/pkg/host"
	"github.com/activecm/rita/pkg/uconnproxy"
	"github.com/activecm/rita/util"

	"github.com/globalsign/mgo"
	"github.com/vbauerster/mpb"
	"github.com/vbauerster/mpb/decor"

	log "github.com/sirupsen/logrus"
)

type repo struct {
	database *database.DB
	config   *config.Config
	log      *log.Logger
}

//NewMongoRepository create new repository
func NewMongoRepository(db *database.DB, conf *config.Config, logger *log.Logger) Repository {
	return &repo{
		database: db,
		config:   conf,
		log:      logger,
	}
}

func (r *repo) CreateIndexes() error {
	session := r.database.Session.Copy()
	defer session.Close()

	// set collection name
	collectionName := r.config.T.BeaconProxy.BeaconProxyTable

	// check if collection already exists
	names, _ := session.DB(r.database.GetSelectedDB()).CollectionNames()

	// if collection exists, we don't need to do anything else
	for _, name := range names {
		if name == collectionName {
			return nil
		}
	}

	// set desired indexes
	indexes := []mgo.Index{
		{Key: []string{"-score"}},
		{Key: []string{"src", "fqdn", "src_network_uuid"}, Unique: true},
		{Key: []string{"src", "src_network_uuid"}},
		{Key: []string{"fqdn"}},
		{Key: []string{"-connection_count"}},
		{Key: []string{"proxy.ip", "proxy.network_uuid"}},
	}

	// create collection
	err := r.database.CreateCollection(collectionName, indexes)
	if err != nil {
		return err
	}

	return nil
}

//Upsert derives beacon statistics from the given unique proxy connections and creates
//summaries for the given local hosts. The results are pushed to MongoDB.
func (r *repo) Upsert(uconnProxyMap map[string]*uconnproxy.Input, hostMap map[string]*host.Input, minTimestamp, maxTimestamp int64) {

	session := r.database.Session.Copy()
	defer session.Close()

	// Create the workers

	// stage 5 - write out results
	writerWorker := newMgoBulkWriter(
		r.database,
		r.config,
		r.log,
		"beaconsProxy",
	)

	// stage 4 - perform the analysis
	analyzerWorker := newAnalyzer(
		minTimestamp,
		maxTimestamp,
		r.config.S.Rolling.CurrentChunk,
		r.database,
		r.config,
		r.log,
		writerWorker.collect,
		writerWorker.close,
	)

	// stage 3 - sort data
	sorterWorker := newSorter(
		r.database,
		r.config,
		analyzerWorker.collect,
		analyzerWorker.close,
	)

	// stage 2 - get and vet beacon details
	dissectorWorker := newDissector(
		int64(r.config.S.Strobe.ConnectionLimit),
		r.database,
		r.config,
		sorterWorker.collect,
		sorterWorker.close,
	)

	// kick off the threaded goroutines
	for i := 0; i < util.Max(1, runtime.NumCPU()/2); i++ {
		dissectorWorker.start()
		sorterWorker.start()
		analyzerWorker.start()
		writerWorker.start()
	}

	// progress bar for troubleshooting
	p := mpb.New(mpb.WithWidth(20))
	bar := p.AddBar(int64(len(uconnProxyMap)),
		mpb.PrependDecorators(
			decor.Name("\t[-] Proxy Beacon Analysis:", decor.WC{W: 30, C: decor.DidentRight}),
			decor.CountersNoUnit(" %d / %d ", decor.WCSyncWidth),
		),
		mpb.AppendDecorators(decor.Percentage()),
	)

	// loop over map entries (each hostname)
	for _, entry := range uconnProxyMap {
		// pass entry to dissector
		dissectorWorker.collect(entry)

		// progress bar increment
		bar.IncrBy(1)

	}
	p.Wait()

	// start the closing cascade (this will also close the other channels)
	dissectorWorker.close()

	// Phase 2: Summary

	// initialize a new writer for the summarizer
	writerWorker = newMgoBulkWriter(r.database, r.config, r.log, "beaconsProxy")
	summarizerWorker := newSummarizer(
		r.config.S.Rolling.CurrentChunk,
		r.database,
		r.config,
		r.log,
		writerWorker.collect,
		writerWorker.close,
	)

	// kick off the threaded goroutines
	for i := 0; i < util.Max(1, runtime.NumCPU()/2); i++ {
		summarizerWorker.start()
		writerWorker.start()
	}

	// grab the local hosts we have seen during the current analysis period
	var localHosts []data.UniqueIP
	for _, entry := range hostMap {
		if entry.IsLocal {
			localHosts = append(localHosts, entry.Host)
		}
	}

	// add a progress bar for troubleshooting
	p = mpb.New(mpb.WithWidth(20))
	bar = p.AddBar(int64(len(localHosts)),
		mpb.PrependDecorators(
			decor.Name("\t[-] Proxy Beacon Aggregation:", decor.WC{W: 30, C: decor.DidentRight}),
			decor.CountersNoUnit(" %d / %d ", decor.WCSyncWidth),
		),
		mpb.AppendDecorators(decor.Percentage()),
	)

	// loop over the local hosts that need to be summarized
	for _, localHost := range localHosts {
		summarizerWorker.collect(localHost)
		bar.IncrBy(1)
	}

	p.Wait()

	// start the closing cascade (this will also close the other channels)
	summarizerWorker.close()
}
