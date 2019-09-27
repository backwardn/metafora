package metafora

import (
	"os"
	"testing"
	"time"
)

func init() {
	if os.Getenv("VERBOSE_TESTS") != "" {
		SetLogger(testlogger{})
	}
}

type testlogger struct{}

func (testlogger) Output(int, string) error { return nil }

// Handler/Consumer test

type testHandler struct {
	stop     chan int
	t        *testing.T
	task     Task
	tasksRun chan string
}

func (h *testHandler) Run() bool {
	h.tasksRun <- h.task.ID()
	h.t.Logf("Run(%s)", h.task.ID())
	<-h.stop
	h.t.Logf("Stop received for %s", h.task.ID())
	return true
}

func (h *testHandler) Stop() {
	h.t.Logf("Stopping %s", h.task.ID())
	close(h.stop)
}

func newTestHandlerFunc(t *testing.T) (HandlerFunc, chan string) {
	tasksRun := make(chan string, 10)
	return func(task Task) Handler {
		return &testHandler{
			task:     task,
			stop:     make(chan int),
			t:        t,
			tasksRun: tasksRun,
		}
	}, tasksRun
}

// TestConsumer ensures the consumers main loop properly handles tasks as well
// as errors and Shutdown.
func TestConsumer(t *testing.T) {
	t.Parallel()

	// Setup some tasks to run in a fake coordinator
	tc := NewTestCoord()
	tc.Tasks <- testTask{"test1"}
	tc.Tasks <- testTask{"test2"}

	// Setup a handler func that lets us know what tasks are running
	hf, tasksRun := newTestHandlerFunc(t)

	// Create the consumer and run it
	c, _ := NewConsumer(tc, hf, DumbBalancer)
	s := make(chan int)
	go func() {
		c.Run()
		s <- 1
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-s:
			t.Fatalf("Run exited early")
		case tr := <-tasksRun:
			if tr != "test1" && tr != "test2" {
				t.Errorf("Expected `test1` or `test2` but received: %s", tr)
			}
			t.Logf("Received task=%q", tr)
		case <-time.After(100 * time.Millisecond):
			t.Errorf("First task didn't execute in a timely fashion")
		}
	}

	// Ensure Tasks() is accurate
	tasks := c.Tasks()
	if len(tasks) != 2 {
		t.Errorf("Expected 2 tasks to be running but found: %v", tasks)
	}

	go func() {
		c.Shutdown()
		s <- 1
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-s:
		case <-time.After(100 * time.Millisecond):
			t.Errorf("Run and Shutdown didn't finish in a timely fashion")
		}
	}
}

// Balancer/Consumer test

type testBalancer struct {
	c         BalancerContext
	t         *testing.T
	secondRun bool
	done      chan struct{}
}

func (b *testBalancer) Init(c BalancerContext) { b.c = c }
func (b *testBalancer) CanClaim(task Task) (time.Time, bool) {
	b.t.Logf("CanClaim(%s) -> %t", task.ID(), task.ID() == "ok-task")
	return time.Now().Add(100 * time.Hour), task.ID() == "ok-task"
}

func (b *testBalancer) Balance() []string {
	if b.secondRun {
		return nil
	}
	b.secondRun = true
	tsks := b.c.Tasks()
	if len(tsks) != 1 {
		b.t.Errorf("len(ConsumerState.Tasks()) != 1 ==> %v", tsks)
		return nil
	}
	if tsks[0].Task().ID() != "ok-task" {
		b.t.Errorf("Wrong task in ConsumerState.Tasks(): %v", tsks)
	}
	close(b.done)
	return nil
}

func TestBalancer(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping due to -short")
	}

	hf, tasksRun := newTestHandlerFunc(t)
	tc := NewTestCoord()
	balDone := make(chan struct{})
	c, _ := NewConsumer(tc, hf, &testBalancer{t: t, done: balDone})
	c.balEvery = 0
	go c.Run()
	tc.Tasks <- testTask{"test1"}
	tc.Tasks <- testTask{"ok-task"}
	tc.Tasks <- testTask{"test2"}

	// Wait for balance
	select {
	case <-balDone:
	case <-time.After(time.Duration(balanceJitterMax) + 10*time.Millisecond):
		t.Error("Didn't balance in a timely fashion")
	}

	select {
	case run := <-tasksRun:
		if run != "ok-task" {
			t.Errorf("Balancer didn't reject tasks properly. Ran task %s", run)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Task didn't run in a timely fashion")
	}

	/*
		if r := c.bal.Balance(); len(r) > 0 {
			t.Errorf("Balance() should return 0, not: %v", r)
		}
	*/

	s := make(chan int)
	go func() {
		c.Shutdown()
		close(s)
	}()
	select {
	case <-s:
	case <-time.After(100 * time.Millisecond):
		t.Errorf("Shutdown didn't finish in a timely fashion")
	}
	if len(c.Tasks()) != 0 {
		t.Errorf("Shutdown didn't stop all tasks")
	}
}

type noopHandler struct{}

func (noopHandler) Run() bool { return true }
func (noopHandler) Stop()     {}

// TestHandleTask ensures that tasks are marked as done once handled.
func TestHandleTask(t *testing.T) {
	hf := func(Task) Handler { return noopHandler{} }
	coord := NewTestCoord()
	c, _ := NewConsumer(coord, hf, DumbBalancer)
	go c.Run()
	coord.Tasks <- testTask{"task1"}
	select {
	case <-coord.Releases:
		t.Errorf("Release called, expected Done!")
	case <-coord.Dones:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("Took too long to mark task as done")
	}
	c.Shutdown()
}

// TestTaskPanic ensures panics from Run methods are turned into Done calls.
func TestTaskPanic(t *testing.T) {
	t.Parallel()
	hf := SimpleHandler(func(Task, <-chan bool) bool {
		panic("TestTaskPanic")
	})
	coord := NewTestCoord()
	c, _ := NewConsumer(coord, hf, DumbBalancer)
	go c.Run()
	coord.Tasks <- testTask{"1"}
	coord.Tasks <- testTask{"2"}
	coord.Tasks <- testTask{"3"}
	for i := 3; i > 0; i-- {
		select {
		case task := <-coord.Dones:
			t.Logf("%s done", task)
		case task := <-coord.Releases:
			t.Errorf("%s released when it should have been marked Done!", task)
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("Took too long to mark task(s) as done.")
		}
	}
	c.Shutdown()
}

// TestShutdown ensures Shutdown causes Run() to exit cleanly.
func TestShutdown(t *testing.T) {
	t.Parallel()
	hf := SimpleHandler(func(_ Task, c <-chan bool) bool {
		<-c
		return false
	})
	coord := NewTestCoord()
	c, _ := NewConsumer(coord, hf, DumbBalancer)
	go c.Run()
	coord.Tasks <- testTask{"1"}
	coord.Tasks <- testTask{"2"}
	coord.Tasks <- testTask{"3"}
	time.Sleep(100 * time.Millisecond)
	if len(coord.Dones)+len(coord.Releases) > 0 {
		t.Fatalf("Didn't expect any tasks to exit before Shutdown was called.")
	}
	c.Shutdown()
	for i := 3; i > 0; i-- {
		select {
		case task := <-coord.Dones:
			t.Errorf("%s makred done when it should have been Released!", task)
		case task := <-coord.Releases:
			t.Logf("%s relased", task)
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("Took too long to mark task(s) as released.")
		}
	}
}
