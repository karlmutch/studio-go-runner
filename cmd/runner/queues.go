package main

// This file contains the implementation of queue query functions along with
// tracking for queues to detect new arrivals and also to detect the
// disappearance of queues
//
// As queues come and go subscriptions are automatically created/accessed so that
// messages have a chance to be noticed

import (
	"context"
	"fmt"
	"math/rand"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SentientTechnologies/studio-go-runner"

	"github.com/davecgh/go-spew/spew"
	"github.com/dustin/go-humanize"

	"github.com/karlmutch/go-cache"

	"github.com/go-stack/stack"
	"github.com/karlmutch/errors"
)

var (
	// backoffs are a set of subscriptions to queues that when they are still alive
	// in the cache the server will not attempt to retrieve work from.  When the
	// cache entries that represent the subscriptions expire then they are
	// deemed to be ready for retrieving more work from.
	//
	// The TTL cache represents the signal to not do something, think of it as a
	// negative signal that has an expiry time.
	//
	// Create a cache with a default expiration time of 1 minute, and which
	// purges expired items every 10 seconds
	//
	backoffs = cache.New(10*time.Second, time.Minute)
)

type SubsBusy struct {
	subs map[string]bool // The catalog of all known queues (subscriptions) within the project this server is handling
	sync.Mutex
}

type Subscription struct {
	name string           // The subscription name that represents a queue of potential for our purposes
	rsc  *runner.Resource // If known the resources that experiments asked for in this subscription
	cnt  uint             // The number of instances that are running for this queue
}

type Subscriptions struct {
	subs map[string]*Subscription // The catalog of all known queues (subscriptions) within the project this server is handling
	sync.Mutex
}

type Queuer struct {
	project string        // The project that is being used to access available work queues
	cred    string        // The credentials file associated with this project
	subs    Subscriptions // The subscriptions that exist within this project
	timeout time.Duration
	tasker  runner.TaskQueue
}

type SubRequest struct {
	project      string
	subscription string
	creds        string
}

func NewQueuer(projectID string, creds string) (qr *Queuer, err errors.Error) {
	qr = &Queuer{
		project: projectID,
		cred:    creds,
		subs:    Subscriptions{subs: map[string]*Subscription{}},
		timeout: 2 * time.Second,
	}
	qr.tasker, err = runner.NewTaskQueue(projectID, creds)
	if err != nil {
		return nil, err
	}
	return qr, nil

}

// refresh is used to update the queuer with a list of available queues
// accessible to the project specified by the queuer
//
func (qr *Queuer) refresh() (err errors.Error) {

	known, err := qr.tasker.Refresh(qr.timeout)
	if err != nil {
		return err
	}

	// Bring the queues collection uptodate with what the system has in terms
	// of functioning queues
	//
	added, removed := qr.subs.align(known)
	msg := ""
	if 0 != len(added) {
		msg += fmt.Sprintf("added queues %s", strings.Join(added, ", "))
	}
	if 0 != len(removed) {
		msg = strings.Join([]string{msg, fmt.Sprintf("removed queues %s", strings.Join(removed, ", "))}, ", and ")
	}
	if 0 != len(msg) {
		msg = fmt.Sprintf("project %s %s", qr.project, msg)
		logger.Info(msg)
		runner.InfoSlack("", msg, []string{})
	}
	return nil
}

// align allows the caller to take the extant subscriptions and add or remove them from the list of subscriptions
// we currently have cached
//
func (subs *Subscriptions) align(expected map[string]interface{}) (added []string, removed []string) {

	added = []string{}
	removed = []string{}

	subs.Lock()
	defer subs.Unlock()

	for sub, _ := range expected {
		if _, isPresent := subs.subs[sub]; !isPresent {

			subs.subs[sub] = &Subscription{name: sub}
			added = append(added, sub)
		}
	}

	for sub, _ := range subs.subs {
		if _, isPresent := expected[sub]; !isPresent {

			delete(subs.subs, sub)
			removed = append(removed, sub)
		}
	}

	return added, removed
}

// setResources is used to update the resources a queue will generally need for
// its individual work items
//
func (subs *Subscriptions) setResources(name string, rsc *runner.Resource) (err errors.Error) {
	if rsc == nil {
		return errors.New(fmt.Sprintf("clearing the resource spec for the subscription %s is not supported", name)).With("stack", stack.Trace().TrimRuntime())
	}

	subs.Lock()
	defer subs.Unlock()

	q, isPresent := subs.subs[name]
	if !isPresent {
		return errors.New(fmt.Sprintf("%s was not present", name)).With("stack", stack.Trace().TrimRuntime())
	}

	q.rsc = rsc

	return nil
}

// shuffles does a fisher-yates shuffle.  This will be introduced in Go 1.10
// as a standard function.  For now we have to do it ourselves. Copied from
// https://gist.github.com/quux00/8258425
//
func shuffle(slc []Subscription) (shuffled []Subscription) {
	n := len(slc)
	for i := 0; i < n; i++ {
		// choose index uniformly in [i, n-1]
		r := i + rand.Intn(n-i)
		slc[r], slc[i] = slc[i], slc[r]
	}
	return slc
}

// producer is used to examine the subscriptions that are available and determine if
// capacity is available to service any of the work that might be waiting
//
func (qr *Queuer) producer(rqst chan *SubRequest, quitC chan bool) {

	logger.Debug("started queue checking producer")
	defer logger.Debug("completed queue checking producer")

	check := time.NewTicker(time.Duration(15 * time.Second))
	defer check.Stop()

	nextQDbg := time.Now()
	lastQs := 0

	lastReady := time.Now()
	lastReadyAbs := time.Now()

	for {
		select {
		case <-check.C:

			ranked := qr.rank()

			// Some monitoring logging used to tracking traffic on queues
			if logger.IsTrace() {
				if len(ranked) != 0 {
					logger.Trace(fmt.Sprintf("processing %s %d ranked subscriptions %s", qr.project, len(ranked), spew.Sdump(ranked)))
				} else {
					logger.Trace(fmt.Sprintf("no %s subscriptions found", qr.project))
				}
			} else {
				if logger.IsDebug() {
					// If either the queue length has changed, or sometime has passed since
					// the last debug log, one minute, print the queue checking state
					if nextQDbg.Before(time.Now()) || lastQs != len(ranked) {
						lastQs = len(ranked)
						nextQDbg = time.Now().Add(10 * time.Minute)
						if len(ranked) != 0 {
							logger.Debug(fmt.Sprintf("processing %d ranked subscriptions %v", len(ranked), ranked))
						} else {
							logger.Debug(fmt.Sprintf("no %s subscriptions found", qr.project))
						}
					}
				}
			}

			// Find queues that are not backed off
			ready := []Subscription{}

			for _, sub := range ranked {
				// Ready queue processing, that is queues that have no work running
				// against this runner
				if sub.cnt == 0 {
					if _, isPresent := backoffs.Get(qr.project + ":" + sub.name); isPresent {
						continue
					}
					// Save the queue that has been waiting the longest into the
					// idle slot that we will be processing on this pass
					ready = append(ready, sub)
				}
			}

			// Shuffle the queues that will be sent in random order to the consumer
			shuffle(ready)

			// Trim the length of the slice of queues to check so that we dont go
			// too crazy against the cloud provider
			if len(ready) > 8 {
				ready = ready[:8]
			}

			for _, readyQ := range ready {
				if err := qr.check(readyQ.name, rqst, quitC); err != nil {

					backoffs.Set(qr.project+":"+readyQ.name, true, time.Duration(time.Minute))

					logger.Warn(fmt.Sprintf("checking %s for work failed due to %s, backoff 1 minute", qr.project+":"+readyQ.name, err.Error()))
					break
				}
				lastReady = time.Now()
				lastReadyAbs = time.Now()
			}

			// Check to see if we were last ready for work more than one hour ago as
			// this could be a resource problem
			if lastReady.Before(time.Now().Add(-1 * time.Hour)) {
				// If we have been unavailable for work alter slack once every 10 minutes and then
				// bump the ready timer for wait for another 10 before resending the advisory
				lastReady = lastReady.Add(10 * time.Minute)
				msg := fmt.Sprintf("no work has been requested by this system for %v, please check for disk space etc resource availability",
					time.Now().Sub(lastReadyAbs))
				runner.WarningSlack("", msg, []string{})
				logger.Warn(msg)
			}
		case <-quitC:
			return
		}
	}
}

func (qr *Queuer) getResources(name string) (rsc *runner.Resource) {
	qr.subs.Lock()
	defer qr.subs.Unlock()

	item, isPresent := qr.subs.subs[name]
	if !isPresent {
		return nil
	}
	return item.rsc.Clone()
}

// Retrieve the queues and count their occupancy, then sort ascending into
// an array
func (qr *Queuer) rank() (ranked []Subscription) {
	qr.subs.Lock()
	defer qr.subs.Unlock()

	ranked = make([]Subscription, 0, len(qr.subs.subs))
	for _, sub := range qr.subs.subs {
		ranked = append(ranked, *sub)
	}

	// sort the queues by their frequency of work, not their occupany of resources
	// so this is approximate but good enough for now
	//
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].cnt < ranked[j].cnt })

	return ranked
}

// getMachineResources extracts the current system state in terms of memory etc
// and coverts this into the resource specification used by jobs.  Because resources
// specified by users are not exact quantities the resource is used for the machines
// resources even in the face of some loss of precision
//
func getMachineResources() (rsc *runner.Resource) {

	rsc = &runner.Resource{}

	// For specified queue look for any free slots on existing GPUs is
	// applicable and fill them, or find empty GPUs and groups to fill
	// in with work

	cpus, v := runner.CPUFree()
	rsc.Cpus = uint(cpus)
	rsc.Ram = humanize.Bytes(v)

	rsc.Hdd = humanize.Bytes(runner.GetDiskFree())

	// go runner allows GPU resources at the board level so obtain the largest single
	// board form factor and use that as our max
	//
	rsc.Gpus = runner.LargestFreeGPUSlots()
	rsc.GpuMem = humanize.Bytes(runner.LargestFreeGPUMem())

	return rsc
}

// check will first validate a subscription and will add it to the list of subscriptions
// to be processed, which is in turn used by the scheduler later.
//
func (qr *Queuer) check(name string, rQ chan *SubRequest, quitC chan bool) (err errors.Error) {

	// fqName is the fully qualified name for the subscription
	key := qr.project + ":" + name

	// Check to see if anyone is listening for a queue to check by sending a dummy request, and then
	// send the real request if the check message is consumed
	select {
	case rQ <- &SubRequest{}:
	default:
		return errors.New("busy checking consumer, at the 1ˢᵗ stage").With("stack", stack.Trace().TrimRuntime())
	}

	sub, isPresent := qr.subs.subs[name]
	if !isPresent {
		return errors.New(key+" subscription could not be found").With("stack", stack.Trace().TrimRuntime())
	}

	if sub.rsc != nil {
		if fit, err := sub.rsc.Fit(getMachineResources()); !fit {
			if err != nil {
				return err
			}

			return errors.New(fmt.Sprintf("%s could not be accomodated %#v -> %#v", key, sub.rsc, getMachineResources())).With("stack", stack.Trace().TrimRuntime())
		} else {
			if logger.IsTrace() {
				logger.Trace(key + " passed capacity check")
			}
		}
	} else {
		if logger.IsTrace() {
			logger.Trace(key + " skipped capacity check")
		}
	}

	select {
	case rQ <- &SubRequest{project: qr.project, subscription: name, creds: qr.cred}:
	case <-time.After(2 * time.Second):
		return errors.New("busy checking consumer, at the 2ⁿᵈ stage").With("stack", stack.Trace().TrimRuntime())
	}

	// Check resource allocation availability to guide fetching work from queues
	// based upon the project ID we have been given
	/**
	gpus := map[string]runner.GPUTrack{}

	// First if we have gpuSlots and mem then look for free gpus slots for
	// the project and if we dont find project specific slots check if
	// we should be using an unassigned device
	if slots != 0 && gpuMem != 0 {
		// Look at GPU devices to see if we can identify bound queues to
		// cards with capacity and fill those, 1 at a time
		gpus = runner.FindGPUs(queue, slots, mem)
		if len(gpus) == 0 {
			gpus = runner.FindGPUs("", slots, mem)
			if len(gpus) == 0 {
				return nil
			}
		}
	}
	**/
	return nil
}

// run will execute maintenance operations in the back ground for the server looking for new
// or old subscriptions and adding them or removing them as needed
//
// This function will block except in the case a fatal issue occurs that prevents it
// from being able to perform the function that it is intended to do
//
func (qr *Queuer) run(quitC chan bool) (err errors.Error) {

	// Start a single unbuffered worker that we have for now to trigger for work
	sendWork := make(chan *SubRequest)
	go qr.consumer(sendWork, quitC)

	// start work producer that looks at subscriptions and then checks the
	// sendWork listener to ensure there is capacity

	go qr.producer(sendWork, quitC)

	refresh := time.Duration(time.Second)

	for {
		select {
		case <-time.After(refresh):
			if err := qr.refresh(); err != nil {
				return err
			}
			refresh = time.Duration(time.Minute)
		case <-quitC:
			return nil
		}
	}
}

func (qr *Queuer) consumer(readyC chan *SubRequest, quitC chan bool) {

	logger.Debug("started checking consumer " + qr.project)
	defer logger.Debug("completed checking consumer " + qr.project)

	for {
		select {
		case request := <-readyC:
			// The channel looks to have been closed so stop handling work
			if request == nil {
				return
			}
			// An empty structure will be sent when the sender want to check if
			// the worker is ready for a scheduling request for a queue
			if len(request.subscription) == 0 {
				continue
			}
			qr.filterWork(request, quitC)
		case <-quitC:
			return
		}
	}
}

// filterWork handles requests to check queues for work.  Before doing the work
// it will however also check to ensure that a backoff time is not in play
// for the queue, if it is then it will simply return
//
func (qr *Queuer) filterWork(request *SubRequest, quitC chan bool) {

	key := request.project + ":" + request.subscription

	// Before spinning off a goroutine check to see if another
	// goroutine is processing this project and queue
	if _, isPresent := backoffs.Get(key); isPresent {
		logger.Trace("backoff on for " + key)
		return
	}

	// When work is started to look at a queue then we want to suppress
	// new activities for some seconds to allow other queues to be serviced
	backoffs.Set(key, true, time.Duration(10*time.Second))

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Warn(fmt.Sprintf("panic in filterWork %#v, %s", r, string(debug.Stack())))
			}
		}()

		qr.doWork(request, quitC)

	}()
}

func (qr *Queuer) doWork(request *SubRequest, quitC chan bool) {

	key := request.project + ":" + request.subscription

	logger.Trace("started checking " + key)

	defer func() {
		if r := recover(); r != nil {
			logger.Warn(fmt.Sprintf("panic running studioml script %#v, %s", r, string(debug.Stack())))
		}
	}()

	// cCtx could be used with a timeout later to have a global limit on runtimes
	cCtx, cCancel := context.WithCancel(context.Background())

	// The cancel is called explicitly below due to GC and defers being delayed

	go func() {
		logger.Trace("started doWork check "+key+" queue timeout is ", qr.timeout.String())
		defer logger.Trace("completed doWork check " + key)

		// Spins out a go routine to handle messages, Work is blocking and will return
		// either after qr.timeout for a work message is received and processed.  Using a 0
		// for maxJobs allows as many jobs to be started as can fill a machine
		cnt, rsc, err := qr.tasker.Work(cCtx, qr.timeout, request.subscription, 0, handleMsg)

		// Cancel the context the message would have been handled using so that the queue
		// checker below wont be activated
		func() {
			defer func() {
				recover()
			}()
			cCancel()
		}()

		if err != nil {
			logger.Warn(key + " msg receive failed due to " + strings.Replace(fmt.Sprint(err), "\n", "", 0))
			return
		} else {
			logger.Info(key + " processed " + strconv.FormatUint(cnt, 10) + " msgs")
		}

		// Set the default resource requirements for the next message fetch to that of the most recently
		// seen resource request
		//

		if rsc != nil {
			if err = qr.subs.setResources(request.subscription, rsc); err != nil {
				logger.Info(key + " resources not updated due to " + err.Error())
			}
		}

	}()

	// While waiting for this check periodically that the queue that
	// was used to send the message still exists, if it does not cancel
	// everything as this is an indication that the work is intended to
	// be stopped in a minute or so
	func() {
		check := time.NewTicker(time.Minute)
		defer check.Stop()

		for {
			select {
			case <-check.C:
				eCtx, eCancel := context.WithTimeout(context.Background(), qr.timeout)
				// Is the queue still there that the job came in on
				exists, err := qr.tasker.Exists(eCtx, request.subscription)
				eCancel()

				if err != nil {
					logger.Info(key + " could not be validated due to " + err.Error())
					continue
				}
				if !exists {
					logger.Warn(key + " no longer found cancelling running tasks")
					// If not cancel the context being used to manage the lifecycle of
					// task processing
					func() {
						defer func() {
							recover()
						}()
						cCancel()
					}()
					return
				}

				// Keep on setting the backoff until the job is done.  The backoff
				// is set to 5 minutes so that if the jobs do complete there is
				// a cooldown period to allow other jobs in
				backoffs.Set(key, true, time.Duration(5*time.Minute))

			case <-cCtx.Done():
				return
			case <-quitC:
				return
			}
		}
	}()

	logger.Trace("completed checking " + key)
}

func handleMsg(ctx context.Context, project string, subscription string, credentials string, msg []byte) (rsc *runner.Resource, consume bool) {

	rsc = nil

	defer func() {
		if r := recover(); r != nil {
			logger.Warn(fmt.Sprintf("%#v", r))
		}
	}()

	key := project + ":" + subscription

	logger.Trace("msg processing started on " + key)
	defer logger.Trace("msg processing completed on " + key)

	// allocate the processor and sub the subscription as
	// the group mechanisim for work comming down the
	// pipe that is sent to the resource allocation
	// module
	proc, err := newProcessor(subscription, msg, credentials, ctx.Done())
	if err != nil {
		logger.Warn("unable to process msg from "+key+" due to %s", err)

		return rsc, true
	}
	defer proc.Close()

	rsc = proc.Request.Experiment.Resource.Clone()

	header := fmt.Sprintf(key + " project " + proc.Request.Config.Database.ProjectId + " experiment " + proc.Request.Experiment.Key)
	logger.Info("started handling " + header)

	// Used to cancel subsequent interactions if the context used by the queue system is cancelled.
	// Timeouts within the processor are not controlled by the queuing system
	prcCtx, prcCancel := context.WithCancel(context.Background())
	// Always cancel the operation, however we should ignore errors as these could
	// be already cancelled so we need to ignore errors at this point
	defer func() {
		defer func() {
			recover()
		}()
		prcCancel()
	}()
	// If the outer context gets cancelled cancel our inner context
	go func() {
		select {
		case <-ctx.Done():
			msg := key + " caller cancelled " + proc.Request.Experiment.Key
			logger.Info(msg)
			prcCancel()
		}
	}()

	// Blocking call to run the entire task and only return on termination due to error or success
	ack, broadcast, err := proc.Process(prcCtx)
	if err != nil {

		if !ack {
			txt := fmt.Sprintf("%s retry due to %s", header, err.Error())
			if broadcast {
				runner.InfoSlack(proc.Request.Config.Runner.SlackDest, txt, []string{})
			}
			logger.Info(txt)
		} else {
			txt := fmt.Sprintf("%s dumped, due to %s", header, err.Error())

			if broadcast {
				runner.WarningSlack(proc.Request.Config.Runner.SlackDest, txt, []string{})
			}
			logger.Warn(txt)
		}
		logger.Warn(err.Error())

		return rsc, ack
	}

	runner.InfoSlack(proc.Request.Config.Runner.SlackDest, header+" stopped", []string{})

	return rsc, ack
}
