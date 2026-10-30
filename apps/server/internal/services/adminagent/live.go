package adminagent

import "sync"

// liveBuffer holds the text of the model turn that is still streaming. It is
// view state only: the finished turn is stored as an event, and a restart
// discards any partial text.
type liveBuffer struct {
	mu      sync.Mutex
	text    string
	version int64
}

func (s *Service) liveFor(runID string) *liveBuffer {
	value, _ := s.live.LoadOrStore(runID, &liveBuffer{})
	buffer, ok := value.(*liveBuffer)
	if !ok {
		return &liveBuffer{}
	}
	return buffer
}

func (s *Service) appendLive(runID, delta string) {
	if delta == "" {
		return
	}
	buffer := s.liveFor(runID)
	buffer.mu.Lock()
	buffer.text += delta
	buffer.version++
	buffer.mu.Unlock()
}

func (s *Service) resetLive(runID string) {
	buffer := s.liveFor(runID)
	buffer.mu.Lock()
	if buffer.text != "" {
		buffer.text = ""
		buffer.version++
	}
	buffer.mu.Unlock()
}

func (s *Service) dropLive(runID string) {
	s.live.Delete(runID)
}

// LiveText returns the partial text of the turn that is still streaming and a
// version that changes whenever the text does.
func (s *Service) LiveText(runID string) (string, int64) {
	value, ok := s.live.Load(runID)
	if !ok {
		return "", 0
	}
	buffer, ok := value.(*liveBuffer)
	if !ok {
		return "", 0
	}
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.text, buffer.version
}
