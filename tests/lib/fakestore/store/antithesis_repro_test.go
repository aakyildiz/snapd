//go:build antithesisrepro

package store

import (
	"fmt"
	"net/http"
	"sync"

	. "gopkg.in/check.v1"
)

// Finding 12 (Antithesis run 9): concurrent requests filled snapsCache
// without a lock and crashed the store with "fatal error: concurrent map
// writes". Run with -race: on the unfixed store the race detector fails the
// test on every run; without -race the crash is only likely.
func (s *storeTestSuite) TestAntithesisFinding12ConcurrentDetailsRequests(c *C) {
	const snaps = 8
	for i := 0; i < snaps; i++ {
		s.makeTestSnap(c, fmt.Sprintf("name: foo%d\nversion: 1", i))
	}
	var wg sync.WaitGroup
	errs := make(chan error, snaps*4)
	for round := 0; round < 4; round++ {
		for i := 0; i < snaps; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				resp, err := s.StoreGet(fmt.Sprintf("/api/v1/snaps/details/foo%d", i))
				if err != nil {
					errs <- err
					return
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					errs <- fmt.Errorf("foo%d: status %d", i, resp.StatusCode)
				}
			}(i)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		c.Check(err, IsNil)
	}
}
