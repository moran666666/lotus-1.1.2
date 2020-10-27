package sectorstorage

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/lotus/extern/sector-storage/sealtasks"
	"github.com/filecoin-project/lotus/extern/sector-storage/storiface"
)

type schedPrioCtxKey int

var SchedPriorityKey schedPrioCtxKey
var DefaultSchedPriority = 0
var SelectorTimeout = 5 * time.Second
var InitWait = 3 * time.Second

var (
	SchedWindows = 2
)

func getPriority(ctx context.Context) int {
	sp := ctx.Value(SchedPriorityKey)
	if p, ok := sp.(int); ok {
		return p
	}

	return DefaultSchedPriority
}

func WithPriority(ctx context.Context, priority int) context.Context {
	return context.WithValue(ctx, SchedPriorityKey, priority)
}

const mib = 1 << 20

type WorkerAction func(ctx context.Context, w Worker) error

type WorkerSelector interface {
	Ok(ctx context.Context, task sealtasks.TaskType, spt abi.RegisteredSealProof, a *workerHandle) (bool, error) // true if worker is acceptable for performing a task

	Cmp(ctx context.Context, task sealtasks.TaskType, a, b *workerHandle) (bool, error) // true if a is preferred over b
	FindDataWoker(ctx context.Context, task sealtasks.TaskType, sid abi.SectorID, spt abi.RegisteredSealProof, a *workerHandle) bool
}

type scheduler struct {
	spt abi.RegisteredSealProof

	workersLk  sync.RWMutex
	nextWorker WorkerID
	workers    map[WorkerID]*workerHandle

	newWorkers chan *workerHandle

	watchClosing  chan WorkerID
	workerClosing chan WorkerID

	schedule       chan *workerRequest
	windowRequests chan *schedWindowRequest

	// owned by the sh.runSched goroutine
	schedQueue  *requestQueue
	openWindows []*schedWindowRequest

	info chan func(interface{})

	closing  chan struct{}
	closed   chan struct{}
	testSync chan struct{} // used for testing
}

type workerHandle struct {
	w Worker

	info storiface.WorkerInfo

	preparing *activeResources
	active    *activeResources

	lk sync.Mutex

	wndLk         sync.Mutex
	activeWindows []*schedWindow

	// stats / tracking
	wt *workTracker

	// for sync manager goroutine closing
	cleanupStarted bool
	closedMgr      chan struct{}
	closingMgr     chan struct{}
	workerOnFree   chan struct{}
}

type schedWindowRequest struct {
	worker WorkerID

	done chan *schedWindow
}

type schedWindow struct {
	allocated activeResources
	todo      []*workerRequest
}

type activeResources struct {
	memUsedMin uint64
	memUsedMax uint64
	gpuUsed    bool
	cpuUse     uint64

	cond *sync.Cond
}

type workerRequest struct {
	sector   abi.SectorID
	taskType sealtasks.TaskType
	priority int // larger values more important
	sel      WorkerSelector

	prepare WorkerAction
	work    WorkerAction

	start time.Time

	index int // The index of the item in the heap.

	indexHeap int
	ret       chan<- workerResponse
	ctx       context.Context
}

type workerResponse struct {
	err error
}

func newScheduler(spt abi.RegisteredSealProof) *scheduler {
	return &scheduler{
		spt: spt,

		nextWorker: 0,
		workers:    map[WorkerID]*workerHandle{},

		newWorkers: make(chan *workerHandle),

		watchClosing:  make(chan WorkerID),
		workerClosing: make(chan WorkerID),

		schedule:       make(chan *workerRequest),
		windowRequests: make(chan *schedWindowRequest, 20),

		schedQueue: &requestQueue{},

		info: make(chan func(interface{})),

		closing: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (sh *scheduler) Schedule(ctx context.Context, sector abi.SectorID, taskType sealtasks.TaskType, sel WorkerSelector, prepare WorkerAction, work WorkerAction) error {
	ret := make(chan workerResponse)

	select {
	case sh.schedule <- &workerRequest{
		sector:   sector,
		taskType: taskType,
		priority: getPriority(ctx),
		sel:      sel,

		prepare: prepare,
		work:    work,

		start: time.Now(),

		ret: ret,
		ctx: ctx,
	}:
	case <-sh.closing:
		return xerrors.New("closing")
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case resp := <-ret:
		return resp.err
	case <-sh.closing:
		return xerrors.New("closing")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *workerRequest) respond(err error) {
	select {
	case r.ret <- workerResponse{err: err}:
	case <-r.ctx.Done():
		log.Warnf("request got cancelled before we could respond")
	}
}

type SchedDiagRequestInfo struct {
	Sector   abi.SectorID
	TaskType sealtasks.TaskType
	Priority int
}

type SchedDiagInfo struct {
	Requests    []SchedDiagRequestInfo
	OpenWindows []WorkerID
}

func (sh *scheduler) runSched() {
	defer close(sh.closed)

	go sh.runWorkerWatcher()
	go func() {
		for {
			time.Sleep(time.Second * 180) // 3分钟执行一次
			sh.workersLk.Lock()
			if sh.schedQueue.Len() > 0 {
				sh.windowRequests <- &schedWindowRequest{}
			}
			sh.workersLk.Unlock()
		}
	}()

	// iw := time.After(InitWait)
	// var initialised bool

	for {
		// var doSched bool

		select {
		case w := <-sh.newWorkers:
			sh.newWorker(w)

		case wid := <-sh.workerClosing:
			sh.dropWorker(wid)

		case req := <-sh.schedule:
			sh.schedQueue.Push(req)
			sh.trySched()

			if sh.testSync != nil {
				sh.testSync <- struct{}{}
			}
		case <-sh.windowRequests:
			// sh.openWindows = append(sh.openWindows, req)
			sh.trySched()
		case ireq := <-sh.info:
			ireq(sh.diag())

		// case <-iw:
		// 	initialised = true
		// 	iw = nil
		// 	doSched = true
		case <-sh.closing:
			sh.schedClose()
			return
		}

		// if doSched && initialised {
		// 	// First gather any pending tasks, so we go through the scheduling loop
		// 	// once for every added task
		// loop:
		// 	for {
		// 		select {
		// 		case req := <-sh.schedule:
		// 			sh.schedQueue.Push(req)
		// 			if sh.testSync != nil {
		// 				sh.testSync <- struct{}{}
		// 			}
		// 		case req := <-sh.windowRequests:
		// 			sh.openWindows = append(sh.openWindows, req)
		// 		default:
		// 			break loop
		// 		}
		// 	}

		// 	sh.trySched()
		// }

	}
}

func (sh *scheduler) diag() SchedDiagInfo {
	var out SchedDiagInfo

	for sqi := 0; sqi < sh.schedQueue.Len(); sqi++ {
		task := (*sh.schedQueue)[sqi]

		out.Requests = append(out.Requests, SchedDiagRequestInfo{
			Sector:   task.sector,
			TaskType: task.taskType,
			Priority: task.priority,
		})
	}

	for _, window := range sh.openWindows {
		out.OpenWindows = append(out.OpenWindows, window.worker)
	}

	return out
}

func (sh *scheduler) trySched() {
	sh.workersLk.Lock()
	defer sh.workersLk.Unlock()

	log.Debugf("trySched %d queued", sh.schedQueue.Len())
	for sqi := 0; sqi < sh.schedQueue.Len(); sqi++ { // 遍历任务列表
		task := (*sh.schedQueue)[sqi]

		tried := 0
		var acceptable []WorkerID
		var freetable []int
		best := 0
		localWorker := false
		for wid, worker := range sh.workers {
			if task.taskType == sealtasks.TTPreCommit2 || task.taskType == sealtasks.TTCommit1 {
				if isExist := task.sel.FindDataWoker(task.ctx, task.taskType, task.sector, sh.spt, worker); !isExist {
					continue
				}
			}

			ok, err := task.sel.Ok(task.ctx, task.taskType, sh.spt, worker)
			if err != nil || !ok {
				continue
			}

			freecount := sh.getTaskFreeCount(wid, task.taskType)
			if freecount <= 0 {
				continue
			}
			tried++
			freetable = append(freetable, freecount)
			acceptable = append(acceptable, wid)

			if isExist := task.sel.FindDataWoker(task.ctx, task.taskType, task.sector, sh.spt, worker); isExist {
				localWorker = true
				break
			}
		}

		if len(acceptable) > 0 {
			if localWorker {
				best = len(acceptable) - 1
			} else {
				max := 0
				for i, v := range freetable {
					if v > max {
						max = v
						best = i
					}
				}
			}

			wid := acceptable[best]
			whl := sh.workers[wid]
			log.Infof("worker %s will be do the %+v jobTask!", whl.info.Hostname, task.taskType)
			sh.schedQueue.Remove(sqi)
			sqi--
			if err := sh.assignWorker(wid, whl, task); err != nil {
				log.Error("assignWorker error: %+v", err)
				go task.respond(xerrors.Errorf("assignWorker error: %w", err))
			}
		}

		if tried == 0 {
			log.Infof("no worker do the %+v jobTask!", task.taskType)
		}
	}
}

func (sh *scheduler) runWorker(wid WorkerID) { // 当新worker加入时由newWorker方法调用运行
	var ready sync.WaitGroup
	ready.Add(1)
	defer ready.Wait()

	go func() {
		sh.workersLk.Lock()
		worker, found := sh.workers[wid] // 检查此id的worker是否存在
		sh.workersLk.Unlock()

		ready.Done()

		if !found {
			panic(fmt.Sprintf("worker %d not found", wid))
		}

		defer close(worker.closedMgr)

		ctx, cancel := context.WithCancel(context.TODO())
		defer cancel()

		workerClosing, err := worker.w.Closing(ctx)
		if err != nil {
			return
		}

		defer func() {
			log.Warnw("Worker closing", "workerid", wid)
		}()

		for {
			sh.windowRequests <- &schedWindowRequest{} // worker空闲申请调度
			select {
			case <-worker.workerOnFree:
				log.Debugw("task done", "workerid", wid)
			case <-sh.closing:
				return
			case <-workerClosing:
				return
			case <-worker.closingMgr:
				return
			}
		}
	}()
}

func (sh *scheduler) workerCompactWindows(worker *workerHandle, wid WorkerID) int {
	// move tasks from older windows to newer windows if older windows
	// still can fit them
	if len(worker.activeWindows) > 1 {
		for wi, window := range worker.activeWindows[1:] {
			lower := worker.activeWindows[wi]
			var moved []int

			for ti, todo := range window.todo {
				needRes := ResourceTable[todo.taskType][sh.spt]
				if !lower.allocated.canHandleRequest(needRes, wid, "compactWindows", worker.info.Resources) {
					continue
				}

				moved = append(moved, ti)
				lower.todo = append(lower.todo, todo)
				lower.allocated.add(worker.info.Resources, needRes)
				window.allocated.free(worker.info.Resources, needRes)
			}

			if len(moved) > 0 {
				newTodo := make([]*workerRequest, 0, len(window.todo)-len(moved))
				for i, t := range window.todo {
					if len(moved) > 0 && moved[0] == i {
						moved = moved[1:]
						continue
					}

					newTodo = append(newTodo, t)
				}
				window.todo = newTodo
			}
		}
	}

	var compacted int
	var newWindows []*schedWindow

	for _, window := range worker.activeWindows {
		if len(window.todo) == 0 {
			compacted++
			continue
		}

		newWindows = append(newWindows, window)
	}

	worker.activeWindows = newWindows

	return compacted
}

func (sh *scheduler) assignWorker(wid WorkerID, w *workerHandle, req *workerRequest) error {
	sh.taskAddOne(wid, req.taskType)
	needRes := ResourceTable[req.taskType][sh.spt]

	w.lk.Lock()
	w.preparing.add(w.info.Resources, needRes)
	w.lk.Unlock()

	go func() {
		defer sh.taskReduceOne(wid, req.taskType)
		err := req.prepare(req.ctx, w.wt.worker(w.w))
		sh.workersLk.Lock()

		if err != nil {
			w.lk.Lock()
			w.preparing.free(w.info.Resources, needRes)
			w.lk.Unlock()
			sh.workersLk.Unlock()

			select {
			case w.workerOnFree <- struct{}{}:
			case <-sh.closing:
				log.Warnf("scheduler closed while sending response (prepare error: %+v)", err)
			}

			select {
			case req.ret <- workerResponse{err: err}:
			case <-req.ctx.Done():
				log.Warnf("request got cancelled before we could respond (prepare error: %+v)", err)
			case <-sh.closing:
				log.Warnf("scheduler closed while sending response (prepare error: %+v)", err)
			}
			return
		}

		err = w.active.withResources(wid, w.info.Resources, needRes, &sh.workersLk, func() error {
			w.lk.Lock()
			w.preparing.free(w.info.Resources, needRes)
			w.lk.Unlock()
			sh.workersLk.Unlock()
			defer sh.workersLk.Lock() // we MUST return locked from this function

			select {
			case w.workerOnFree <- struct{}{}:
			case <-sh.closing:
			}

			err = req.work(req.ctx, w.wt.worker(w.w))

			select {
			case req.ret <- workerResponse{err: err}:
			case <-req.ctx.Done():
				log.Warnf("request got cancelled before we could respond")
			case <-sh.closing:
				log.Warnf("scheduler closed while sending response")
			}

			return nil
		})

		sh.workersLk.Unlock()

		// This error should always be nil, since nothing is setting it, but just to be safe:
		if err != nil {
			log.Errorf("error executing worker (withResources): %+v", err)
		}
	}()

	return nil
}

func (sh *scheduler) newWorker(w *workerHandle) {
	w.closedMgr = make(chan struct{})
	w.closingMgr = make(chan struct{})
	w.workerOnFree = make(chan struct{})

	sh.workersLk.Lock()

	id := sh.nextWorker
	sh.workers[id] = w
	sh.nextWorker++

	sh.workersLk.Unlock()

	sh.runWorker(id)

	select {
	case sh.watchClosing <- id:
	case <-sh.closing:
		return
	}
}

func (sh *scheduler) dropWorker(wid WorkerID) {
	sh.workersLk.Lock()
	defer sh.workersLk.Unlock()

	w := sh.workers[wid]

	sh.workerCleanup(wid, w)

	delete(sh.workers, wid)
}

func (sh *scheduler) workerCleanup(wid WorkerID, w *workerHandle) {
	select {
	case <-w.closingMgr:
	default:
		close(w.closingMgr)
	}

	sh.workersLk.Unlock()
	select {
	case <-w.closedMgr:
	case <-time.After(time.Second):
		log.Errorf("timeout closing worker manager goroutine %d", wid)
	}
	sh.workersLk.Lock()

	if !w.cleanupStarted {
		w.cleanupStarted = true

		newWindows := make([]*schedWindowRequest, 0, len(sh.openWindows))
		for _, window := range sh.openWindows {
			if window.worker != wid {
				newWindows = append(newWindows, window)
			}
		}
		sh.openWindows = newWindows

		log.Debugf("dropWorker %d", wid)

		go func() {
			if err := w.w.Close(); err != nil {
				log.Warnf("closing worker %d: %+v", err)
			}
		}()
	}
}

func (sh *scheduler) schedClose() {
	sh.workersLk.Lock()
	defer sh.workersLk.Unlock()
	log.Debugf("closing scheduler")

	for i, w := range sh.workers {
		sh.workerCleanup(i, w)
	}
}

func (sh *scheduler) Info(ctx context.Context) (interface{}, error) {
	ch := make(chan interface{}, 1)

	sh.info <- func(res interface{}) {
		ch <- res
	}

	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (sh *scheduler) Close(ctx context.Context) error {
	close(sh.closing)
	select {
	case <-sh.closed:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (sh *scheduler) taskAddOne(wid WorkerID, phaseTaskType sealtasks.TaskType) {
	if whl, ok := sh.workers[wid]; ok {
		whl.info.TaskResourcesLk.Lock()
		defer whl.info.TaskResourcesLk.Unlock()
		if counts, ok := whl.info.TaskResources[phaseTaskType]; ok {
			counts.RunCount++
		}
	}
}

func (sh *scheduler) taskReduceOne(wid WorkerID, phaseTaskType sealtasks.TaskType) {
	if whl, ok := sh.workers[wid]; ok {
		whl.info.TaskResourcesLk.Lock()
		defer whl.info.TaskResourcesLk.Unlock()
		if counts, ok := whl.info.TaskResources[phaseTaskType]; ok {
			counts.RunCount--
		}
	}
}

func (sh *scheduler) getTaskCount(wid WorkerID, phaseTaskType sealtasks.TaskType, typeCount string) int {
	if whl, ok := sh.workers[wid]; ok {
		if counts, ok := whl.info.TaskResources[phaseTaskType]; ok {
			whl.info.TaskResourcesLk.Lock()
			defer whl.info.TaskResourcesLk.Unlock()
			if typeCount == "limit" {
				return counts.LimitCount
			}
			if typeCount == "run" {
				return counts.RunCount
			}
		}
	}
	return 0
}

func (sh *scheduler) getTaskFreeCount(wid WorkerID, phaseTaskType sealtasks.TaskType) int {
	limitCount := sh.getTaskCount(wid, phaseTaskType, "limit") // json文件限制的任务数量
	runCount := sh.getTaskCount(wid, phaseTaskType, "run")     // 运行中的任务数量
	freeCount := limitCount - runCount

	if limitCount == 0 { // 0:禁止
		return 0
	}

	whl := sh.workers[wid]
	log.Infof("worker %s %s: %d free count", whl.info.Hostname, phaseTaskType, freeCount)

	if phaseTaskType == sealtasks.TTAddPiece || phaseTaskType == sealtasks.TTPreCommit1 {
		if freeCount >= 0 { // 空闲数量不小于0，小于0也要校准为0
			return freeCount
		}
		return 0
	}

	if phaseTaskType == sealtasks.TTPreCommit2 || phaseTaskType == sealtasks.TTCommit1 {
		c2runCount := sh.getTaskCount(wid, sealtasks.TTCommit2, "run")
		if freeCount >= 0 && c2runCount <= 0 { // 需做的任务空闲数量不小于0，且没有c2任务在运行
			return freeCount
		}
		log.Infof("worker already doing C2 taskjob")
		return 0
	}

	if phaseTaskType == sealtasks.TTCommit2 {
		p2runCount := sh.getTaskCount(wid, sealtasks.TTPreCommit2, "run")
		c1runCount := sh.getTaskCount(wid, sealtasks.TTCommit1, "run")
		if freeCount >= 0 && p2runCount <= 0 && c1runCount <= 0 { // 需做的任务空闲数量不小于0，且没有p2\c1任务在运行
			return freeCount
		}
		log.Infof("worker already doing P2C1 taskjob")
		return 0
	}

	if phaseTaskType == sealtasks.TTFetch || phaseTaskType == sealtasks.TTFinalize ||
		phaseTaskType == sealtasks.TTUnseal || phaseTaskType == sealtasks.TTReadUnsealed { // 不限制
		return 1
	}

	return 0
}
