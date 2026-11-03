package copier

import "sync"

// workQueue is a list of work that can grow while workers are already
// taking from it, and is closed when nothing more will be added. (A channel
// can't do both: a producer that finds it full would block, and nobody can
// tell when "no more producers" has happened.) Used by the phone pull lane
// (the tar lane hands it what a stream didn't deliver) and by local copies
// (the reconciling sweep hands it files the index missed).
type workQueue[T any] struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []T
	closed bool
}

func newWorkQueue[T any]() *workQueue[T] {
	q := &workQueue[T]{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// push adds work (ignored after close).
func (q *workQueue[T]) push(v ...T) {
	q.mu.Lock()
	if !q.closed {
		q.items = append(q.items, v...)
	}
	q.mu.Unlock()
	q.cond.Broadcast()
}

// close says no more work will come.
func (q *workQueue[T]) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// pop waits for work; ok is false once the queue is closed and empty.
func (q *workQueue[T]) pop() (v T, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return v, false
	}
	v = q.items[0]
	var zero T
	q.items[0] = zero
	q.items = q.items[1:]
	return v, true
}
