package mpb

import (
	"errors"
	"io"
	"log"
	"os"
	"runtime"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/vbauerster/mpb/cwriter"
)

var logger = log.New(os.Stderr, "mpb: ", log.LstdFlags|log.Lshortfile)

// ErrCallAfterStop thrown by panic, if Progress methods like (*Progress).AddBar()
// are called after (*Progress).Stop() has been called
var ErrCallAfterStop = errors.New("method call on stopped Progress instance")

type (
	// BeforeRender is a func, which gets called before render process
	BeforeRender func([]*Bar)
	barOpType    uint

	operation struct {
		kind   barOpType
		bar    *Bar
		result chan bool
	}

	indexedBarBuffer struct {
		index int
		buf   []byte
	}

	indexedBar struct {
		index     int
		termWidth int
		bar       *Bar
	}

	widthSync struct {
		listen []chan int
		result []chan int
	}
)

const (
	barAdd barOpType = iota
	barRemove
)

const (
	// default RefreshRate
	rr = 100
	// default width
	pwidth = 70
	// number of format runes for bar
	numFmtRunes = 5
)

// Progress represents the container that renders Progress bars
type Progress struct {
	// Context for canceling bars rendering
	// ctx context.Context
	// WaitGroup for internal rendering sync
	wg *sync.WaitGroup

	out    io.Writer
	width  int
	format string

	operationCh    chan *operation
	rrChangeReqCh  chan time.Duration
	outChangeReqCh chan io.Writer
	barCountReqCh  chan chan int
	brCh           chan BeforeRender
	done           chan struct{}
	cancel         <-chan struct{}
}

// New creates new Progress instance, which will orchestrate bars rendering
// process. It acceepts context.Context, for cancellation.
// If you don't plan to cancel, it is safe to feed with nil
func New() *Progress {
	p := &Progress{
		width:          pwidth,
		operationCh:    make(chan *operation),
		rrChangeReqCh:  make(chan time.Duration),
		outChangeReqCh: make(chan io.Writer),
		barCountReqCh:  make(chan chan int),
		brCh:           make(chan BeforeRender),
		done:           make(chan struct{}),
		wg:             new(sync.WaitGroup),
	}
	go p.server()
	return p
}

// WithCancel cancellation via channel
func (p *Progress) WithCancel(ch <-chan struct{}) *Progress {
	if ch == nil {
		panic("nil cancel channel")
	}
	p2 := new(Progress)
	*p2 = *p
	p2.cancel = ch
	return p2
}

// SetWidth overrides default (70) width of bar(s)
func (p *Progress) SetWidth(n int) *Progress {
	if n < 0 {
		panic("negative width")
	}
	p2 := new(Progress)
	*p2 = *p
	p2.width = n
	return p2
}

// SetOut sets underlying writer of progress. Default is os.Stdout
// pancis, if called on stopped Progress instance, i.e after (*Progress).Stop()
func (p *Progress) SetOut(w io.Writer) *Progress {
	if isClosed(p.done) {
		panic(ErrCallAfterStop)
	}
	if w == nil {
		return p
	}
	p.outChangeReqCh <- w
	return p
}

// RefreshRate overrides default (100ms) refresh rate value
// pancis, if called on stopped Progress instance, i.e after (*Progress).Stop()
func (p *Progress) RefreshRate(d time.Duration) *Progress {
	if isClosed(p.done) {
		panic(ErrCallAfterStop)
	}
	p.rrChangeReqCh <- d
	return p
}

// BeforeRenderFunc accepts a func, which gets called before render process.
func (p *Progress) BeforeRenderFunc(f BeforeRender) *Progress {
	if isClosed(p.done) {
		panic(ErrCallAfterStop)
	}
	p.brCh <- f
	return p
}

// AddBar creates a new progress bar and adds to the container
// pancis, if called on stopped Progress instance, i.e after (*Progress).Stop()
func (p *Progress) AddBar(total int64) *Bar {
	return p.AddBarWithID(0, total)
}

// AddBarWithID creates a new progress bar and adds to the container
// pancis, if called on stopped Progress instance, i.e after (*Progress).Stop()
func (p *Progress) AddBarWithID(id int, total int64) *Bar {
	if isClosed(p.done) {
		panic(ErrCallAfterStop)
	}
	result := make(chan bool)
	bar := newBar(id, total, p.width, p.format, p.wg, p.cancel)
	p.operationCh <- &operation{barAdd, bar, result}
	if <-result {
		p.wg.Add(1)
	}
	return bar
}

// RemoveBar removes bar at any time.
// Pancis, if called on stopped Progress instance, i.e after (*Progress).Stop()
func (p *Progress) RemoveBar(b *Bar) bool {
	if isClosed(p.done) {
		panic(ErrCallAfterStop)
	}
	result := make(chan bool)
	p.operationCh <- &operation{barRemove, b, result}
	return <-result
}

// BarCount returns bars count in the container.
// Pancis if called on stopped Progress instance, i.e after (*Progress).Stop()
func (p *Progress) BarCount() int {
	if isClosed(p.done) {
		panic(ErrCallAfterStop)
	}
	respCh := make(chan int, 1)
	p.barCountReqCh <- respCh
	return <-respCh
}

// Format sets custom format for underlying bar(s).
// The default one is "[=>-]"
func (p *Progress) Format(format string) *Progress {
	if utf8.RuneCountInString(format) != numFmtRunes {
		return p
	}
	p.format = format
	return p
}

// Stop shutdowns Progress' goroutine.
// Should be called only after each bar's work done, i.e. bar has reached its
// 100 %. It is NOT for cancelation. Use WithContext or WithCancel for
// cancelation purposes.
func (p *Progress) Stop() {
	p.wg.Wait()
	if isClosed(p.done) {
		return
	}
	close(p.operationCh)
}

// server monitors underlying channels and renders any progress bars
func (p *Progress) server() {
	userRR := rr * time.Millisecond
	t := time.NewTicker(userRR)

	defer func() {
		t.Stop()
		close(p.done)
	}()

	var wg sync.WaitGroup
	recoverIfPanic := func() {
		if p := recover(); p != nil {
			logger.Printf("unexpected panic: %+v\n", p)
			var buf [4096]byte
			n := runtime.Stack(buf[:], false)
			os.Stderr.Write(buf[:n])
		}
		wg.Done()
	}

	var beforeRender BeforeRender
	cw := cwriter.New(os.Stdout)
	bars := make([]*Bar, 0, 3)

	for {
		select {
		case w := <-p.outChangeReqCh:
			cw.Flush()
			cw = cwriter.New(w)
		case op, ok := <-p.operationCh:
			if !ok {
				return
			}
			switch op.kind {
			case barAdd:
				bars = append(bars, op.bar)
				op.result <- true
			case barRemove:
				var ok bool
				for i, b := range bars {
					if b == op.bar {
						bars = append(bars[:i], bars[i+1:]...)
						ok = true
						b.remove()
						break
					}
				}
				op.result <- ok
			}
		case respCh := <-p.barCountReqCh:
			respCh <- len(bars)
		case beforeRender = <-p.brCh:
		case <-t.C:
			numBars := len(bars)

			if numBars == 0 {
				break
			}

			if beforeRender != nil {
				beforeRender(bars)
			}

			quitWidthSyncCh := make(chan struct{})
			time.AfterFunc(userRR, func() {
				close(quitWidthSyncCh)
			})

			b0 := bars[0]
			prependWs := newWidthSync(quitWidthSyncCh, numBars, b0.NumOfPrependers())
			appendWs := newWidthSync(quitWidthSyncCh, numBars, b0.NumOfAppenders())

			width, _, _ := cwriter.GetTermSize()
			ibars := iBarsGen(bars, width)
			ibbCh := make(chan indexedBarBuffer)
			wg.Add(numBars)
			for i := 0; i < numBars; i++ {
				go func() {
					defer recoverIfPanic()
					drawer(ibars, ibbCh, prependWs, appendWs)
				}()
			}
			go func() {
				wg.Wait()
				close(ibbCh)
				for _, ch := range prependWs.listen {
					close(ch)
				}
				for _, ch := range appendWs.listen {
					close(ch)
				}
			}()

			m := make(map[int][]byte, len(bars))
			for ibb := range ibbCh {
				m[ibb.index] = ibb.buf
			}
			for i := 0; i < len(bars); i++ {
				cw.Write(m[i])
			}

			cw.Flush()

			for _, b := range bars {
				b.flushed()
			}
		case userRR = <-p.rrChangeReqCh:
			t.Stop()
			t = time.NewTicker(userRR)
		case <-p.cancel:
			return
		}
	}
}

func newWidthSync(quit <-chan struct{}, numBars, numColumn int) *widthSync {
	ws := &widthSync{
		listen: make([]chan int, numColumn),
		result: make([]chan int, numColumn),
	}
	for i := 0; i < numColumn; i++ {
		ws.listen[i] = make(chan int, numBars)
		ws.result[i] = make(chan int, numBars)
	}
	for i := 0; i < numColumn; i++ {
		go func(listenCh <-chan int, resultCh chan<- int) {
			defer close(resultCh)
			widths := make([]int, 0, numBars)
		loop:
			for {
				select {
				case w := <-listenCh:
					widths = append(widths, w)
					if len(widths) == numBars {
						break loop
					}
				case <-quit:
					if len(widths) == 0 {
						return
					}
					break loop
				}
			}
			result := max(widths)
			for i := 0; i < len(widths); i++ {
				resultCh <- result
			}
		}(ws.listen[i], ws.result[i])
	}
	return ws
}

func drawer(ibars <-chan indexedBar, ibbCh chan<- indexedBarBuffer, prependWs, appendWs *widthSync) {
	for b := range ibars {
		buf := b.bar.bytes(b.termWidth, prependWs, appendWs)
		buf = append(buf, '\n')
		ibbCh <- indexedBarBuffer{b.index, buf}
	}
}

func iBarsGen(bars []*Bar, width int) <-chan indexedBar {
	ibars := make(chan indexedBar)
	go func() {
		defer close(ibars)
		for i, b := range bars {
			ibars <- indexedBar{i, width, b}
		}
	}()
	return ibars
}

// isClosed check if ch closed
// caution see: http://www.tapirgames.com/blog/golang-channel-closing
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func max(slice []int) int {
	max := slice[0]

	for i := 1; i < len(slice); i++ {
		if slice[i] > max {
			max = slice[i]
		}
	}

	return max
}
