package beaconfqdn

import (
	"math"
	"sort"
	"sync"

	"github.com/activecm/rita/config"
	"github.com/activecm/rita/database"
	"github.com/activecm/rita/pkg/data"
	"github.com/activecm/rita/util"
	"github.com/globalsign/mgo/bson"
	log "github.com/sirupsen/logrus"
)

type (
	//analyzer handles calculating statistical measures of the distributions of the
	//timestamps and data sizes from one host to the set of hosts associated with an fqdn
	analyzer struct {
		tsMin            int64           // min timestamp for the whole dataset
		tsMax            int64           // max timestamp for the whole dataset
		chunk            int             // current chunk (0 if not on rolling analysis)
		db               *database.DB    // provides access to MongoDB
		conf             *config.Config  // contains details needed to access MongoDB
		log              *log.Logger     // main logger for RITA
		analyzedCallback func(update)    // called on each analyzed result
		closedCallback   func()          // called when .close() is called and no more calls to analyzedCallback will be made
		analysisChannel  chan *fqdnInput // holds unanalyzed data
		analysisWg       sync.WaitGroup  // wait for analysis to finish
	}
)

//newAnalyzer creates a new analyzer for calculating the beacon statistics of src IP -> fqdn connections
func newAnalyzer(min int64, max int64, chunk int, db *database.DB, conf *config.Config, log *log.Logger,
	analyzedCallback func(update), closedCallback func()) *analyzer {
	return &analyzer{
		tsMin:            min,
		tsMax:            max,
		chunk:            chunk,
		db:               db,
		conf:             conf,
		log:              log,
		analyzedCallback: analyzedCallback,
		closedCallback:   closedCallback,
		analysisChannel:  make(chan *fqdnInput),
	}
}

//collect gathers sorted src -> fqdn connection data for analysis
func (a *analyzer) collect(data *fqdnInput) {
	a.analysisChannel <- data
}

//close waits for the analyzer to finish
func (a *analyzer) close() {
	close(a.analysisChannel)
	a.analysisWg.Wait()
	a.closedCallback()
}

//start kicks off a new analysis thread
func (a *analyzer) start() {
	a.analysisWg.Add(1)

	go func() {
		for entry := range a.analysisChannel {

			// create selector pair object
			selectorPair := data.UniqueSrcFQDNPair{
				UniqueSrcIP: entry.Src,
				FQDN:        entry.FQDN,
			}

			// create query
			query := bson.M{}

			// if beacon has turned into a strobe, we will not have any timestamps here,
			// and need to update beaconFQDN table with the strobeFQDN flag.
			if (entry.TsList) == nil {
				// set strobe info
				query["$set"] = bson.M{
					"strobeFQDN":       true,
					"total_bytes":      entry.TotalBytes,
					"avg_bytes":        entry.TotalBytes / entry.ConnectionCount,
					"connection_count": entry.ConnectionCount,
					"src_network_name": entry.Src.SrcNetworkName,
					"resolved_ips":     entry.ResolvedIPs,
					"cid":              a.chunk,
				}

				// unset any beacon calculations since  this
				// is now a strobe and those would be inaccurate
				// (this will only apply to chunked imports)
				query["$unset"] = bson.M{
					"ts":    1,
					"ds":    1,
					"score": 1,
				}

				a.analyzedCallback(update{
					selector: selectorPair.BSONKey(),
					query:    query,
				})

			} else {
				//store the diff slice length since we use it a lot
				//for timestamps this is one less then the data slice length
				//since we are calculating the times in between readings
				tsLength := len(entry.TsList) - 1
				dsLength := len(entry.OrigBytesList)

				//find the delta times between the timestamps
				diff := make([]int64, tsLength)
				for i := 0; i < tsLength; i++ {
					diff[i] = entry.TsList[i+1] - entry.TsList[i]
				}

				//find the delta times between full list of timestamps
				//(this will be used for the intervals list. Bowleys skew
				//must use a unique timestamp list with no duplicates)
				tsLengthFull := len(entry.TsListFull) - 1
				//find the delta times between the timestamps
				diffFull := make([]int64, tsLengthFull)
				for i := 0; i < tsLengthFull; i++ {
					diffFull[i] = entry.TsListFull[i+1] - entry.TsListFull[i]
				}

				//perfect beacons should have symmetric delta time and size distributions
				//Bowley's measure of skew is used to check symmetry
				sort.Sort(util.SortableInt64(diff))
				tsSkew := float64(0)
				dsSkew := float64(0)

				//tsLength -1 is used since diff is a zero based slice
				tsLow := diff[util.Round(.25*float64(tsLength-1))]
				tsMid := diff[util.Round(.5*float64(tsLength-1))]
				tsHigh := diff[util.Round(.75*float64(tsLength-1))]
				tsBowleyNum := tsLow + tsHigh - 2*tsMid
				tsBowleyDen := tsHigh - tsLow

				//we do the same for datasizes
				dsLow := entry.OrigBytesList[util.Round(.25*float64(dsLength-1))]
				dsMid := entry.OrigBytesList[util.Round(.5*float64(dsLength-1))]
				dsHigh := entry.OrigBytesList[util.Round(.75*float64(dsLength-1))]
				dsBowleyNum := dsLow + dsHigh - 2*dsMid
				dsBowleyDen := dsHigh - dsLow

				//tsSkew should equal zero if the denominator equals zero
				//bowley skew is unreliable if Q2 = Q1 or Q2 = Q3
				if tsBowleyDen != 0 && tsMid != tsLow && tsMid != tsHigh {
					tsSkew = float64(tsBowleyNum) / float64(tsBowleyDen)
				}

				if dsBowleyDen != 0 && dsMid != dsLow && dsMid != dsHigh {
					dsSkew = float64(dsBowleyNum) / float64(dsBowleyDen)
				}

				//perfect beacons should have very low dispersion around the
				//median of their delta times
				//Median Absolute Deviation About the Median
				//is used to check dispersion
				devs := make([]int64, tsLength)
				for i := 0; i < tsLength; i++ {
					devs[i] = util.Abs(diff[i] - tsMid)
				}

				dsDevs := make([]int64, dsLength)
				for i := 0; i < dsLength; i++ {
					dsDevs[i] = util.Abs(entry.OrigBytesList[i] - dsMid)
				}

				sort.Sort(util.SortableInt64(devs))
				sort.Sort(util.SortableInt64(dsDevs))

				tsMadm := devs[util.Round(.5*float64(tsLength-1))]
				dsMadm := dsDevs[util.Round(.5*float64(dsLength-1))]

				//Store the range for human analysis
				tsIntervalRange := diff[tsLength-1] - diff[0]
				dsRange := entry.OrigBytesList[dsLength-1] - entry.OrigBytesList[0]

				//get a list of the intervals found in the data,
				//the number of times the interval was found,
				//and the most occurring interval
				//sort intervals list (origbytes already sorted)
				sort.Sort(util.SortableInt64(diffFull))
				intervals, intervalCounts, tsMode, tsModeCount := createCountMap(diffFull)
				dsSizes, dsCounts, dsMode, dsModeCount := createCountMap(entry.OrigBytesList)

				//more skewed distributions receive a lower score
				//less skewed distributions receive a higher score
				tsSkewScore := 1.0 - math.Abs(tsSkew) //smush tsSkew
				dsSkewScore := 1.0 - math.Abs(dsSkew) //smush dsSkew

				//lower dispersion is better, cutoff dispersion scores at 30 seconds
				tsMadmScore := 1.0 - float64(tsMadm)/30.0
				if tsMadmScore < 0 {
					tsMadmScore = 0
				}

				//lower dispersion is better, cutoff dispersion scores at 32 bytes
				dsMadmScore := 1.0 - float64(dsMadm)/32.0
				if dsMadmScore < 0 {
					dsMadmScore = 0
				}

				//smaller data sizes receive a higher score
				dsSmallnessScore := 1.0 - float64(dsMode)/65535.0
				if dsSmallnessScore < 0 {
					dsSmallnessScore = 0
				}

				// connection count scoring
				tsConnDiv := (float64(a.tsMax) - float64(a.tsMin)) / 10.0
				tsConnCountScore := float64(entry.ConnectionCount) / tsConnDiv
				if tsConnCountScore > 1.0 {
					tsConnCountScore = 1.0
				}

				//score numerators
				tsSum := tsSkewScore + tsMadmScore + tsConnCountScore
				dsSum := dsSkewScore + dsMadmScore + dsSmallnessScore

				//score averages
				tsScore := math.Ceil((tsSum/3.0)*1000) / 1000
				dsScore := math.Ceil((dsSum/3.0)*1000) / 1000
				score := math.Ceil(((tsSum+dsSum)/6.0)*1000) / 1000

				// update beacon query
				query["$set"] = bson.M{
					"connection_count":   entry.ConnectionCount,
					"avg_bytes":          entry.TotalBytes / entry.ConnectionCount,
					"ts.range":           tsIntervalRange,
					"ts.mode":            tsMode,
					"ts.mode_count":      tsModeCount,
					"ts.intervals":       intervals,
					"ts.interval_counts": intervalCounts,
					"ts.dispersion":      tsMadm,
					"ts.skew":            tsSkew,
					"ts.conns_score":     tsConnCountScore,
					"ts.score":           tsScore,
					"ds.range":           dsRange,
					"ds.mode":            dsMode,
					"ds.mode_count":      dsModeCount,
					"ds.sizes":           dsSizes,
					"ds.counts":          dsCounts,
					"ds.dispersion":      dsMadm,
					"ds.skew":            dsSkew,
					"ds.score":           dsScore,
					"score":              score,
					"cid":                a.chunk,
					"src_network_name":   entry.Src.SrcNetworkName,
					"resolved_ips":       entry.ResolvedIPs,
					"strobeFQDN":         false,
				}

				a.analyzedCallback(update{
					selector: selectorPair.BSONKey(),
					query:    query,
				})
			}
		}

		a.analysisWg.Done()
	}()
}

// createCountMap returns a distinct data array, data count array, the mode,
// and the number of times the mode occurred
func createCountMap(sortedIn []int64) ([]int64, []int64, int64, int64) {
	//Since the data is already sorted, we can call this without fear
	distinct, countsMap := countAndRemoveConsecutiveDuplicates(sortedIn)
	countsArr := make([]int64, len(distinct))
	mode := distinct[0]
	max := countsMap[mode]
	for i, datum := range distinct {
		count := countsMap[datum]
		countsArr[i] = count
		if count > max {
			max = count
			mode = datum
		}
	}
	return distinct, countsArr, mode, max
}

//countAndRemoveConsecutiveDuplicates removes consecutive
//duplicates in an array of integers and counts how many
//instances of each number exist in the array.
//Similar to `uniq -c`, but counts all duplicates, not just
//consecutive duplicates.
func countAndRemoveConsecutiveDuplicates(numberList []int64) ([]int64, map[int64]int64) {
	//Avoid some reallocations
	result := make([]int64, 0, len(numberList)/2)
	counts := make(map[int64]int64)

	last := numberList[0]
	result = append(result, last)
	counts[last]++

	for idx := 1; idx < len(numberList); idx++ {
		if last != numberList[idx] {
			result = append(result, numberList[idx])
		}
		last = numberList[idx]
		counts[last]++
	}
	return result, counts
}
