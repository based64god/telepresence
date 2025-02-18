package integration_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/v2/integration_test/itest"
)

type multipleServicesSuite struct {
	itest.Suite
	itest.MultipleServices
}

func (s *multipleServicesSuite) SuiteName() string {
	return "MultipleServices"
}

func init() {
	itest.AddMultipleServicesSuite("", "hello", func(h itest.MultipleServices) itest.TestingSuite {
		return &multipleServicesSuite{Suite: itest.Suite{Harness: h}, MultipleServices: h}
	})
}

func (s *multipleServicesSuite) Test_LargeRequest() {
	client := &http.Client{Timeout: 15 * time.Minute}
	const sendSize = 1024 * 1024 * 20
	const varyMax = 1 << 15 // vary last 64Ki
	const concurrentRequests = 13

	tb := [sendSize + varyMax]byte{}
	tb[0] = '!'
	tb[1] = '\n'
	for i := 2; i < sendSize+varyMax; i++ {
		tb[i] = 'A'
	}

	time.Sleep(3 * time.Second)
	wg := sync.WaitGroup{}
	wg.Add(concurrentRequests)
	for i := 0; i < concurrentRequests; i++ {
		go func(x int) {
			defer wg.Done()
			sendSize := sendSize + rand.Int()%varyMax // vary the last 64Ki to get random buffer sizes
			b := tb[:sendSize]

			// Distribute the requests over all services
			url := fmt.Sprintf("http://%s-%d.%s/put", s.Name(), x%s.ServiceCount(), s.AppNamespace())
			req, err := http.NewRequest(http.MethodPut, url, bytes.NewBuffer(b))
			if !s.NoError(err) {
				return
			}

			resp, err := client.Do(req)
			if !s.NoError(err) {
				return
			}
			bdy := resp.Body
			defer bdy.Close()
			if !s.Equal(resp.StatusCode, 200) {
				return
			}

			cl := sendSize + 1024
			buf := make([]byte, cl)
			i := 0
			for i < cl && err == nil {
				var j int
				j, err = bdy.Read(buf[i:])
				i += j
			}
			if errors.Is(err, io.EOF) {
				err = nil
			}
			if s.NoError(err) {
				ei := bytes.Index(buf, []byte{'!', '\n'})
				s.GreaterOrEqual(ei, 0)
				// Do this instead of require.Equal(b, buf[ei:i]) so that on failure we don't print two very large buffers to the terminal
				s.Equal(true, bytes.Equal(b, buf[ei:i]))
			}
		}(i)
	}
	wg.Wait()
}

func (s *multipleServicesSuite) Test_List() {
	stdout := itest.TelepresenceOk(s.Context(), "-n", s.AppNamespace(), "list")
	for i := 0; i < s.ServiceCount(); i++ {
		s.Regexp(fmt.Sprintf(`%s-%d\s*: ready to intercept`, s.Name(), i), stdout)
	}
}

func (s *multipleServicesSuite) Test_ListOnlyMapped() {
	ctx := itest.WithUser(s.Context(), "default")
	require := s.Require()
	itest.TelepresenceDisconnectOk(ctx)
	defer func() {
		ctx := s.Context()
		itest.TelepresenceDisconnectOk(ctx)
		itest.TelepresenceOk(s.Context(), "connect", "--namespace", s.AppNamespace(), "--manager-namespace", s.ManagerNamespace())
	}()
	s.TelepresenceConnect(ctx, "--mapped-namespaces", "default")

	stdout := itest.TelepresenceOk(ctx, "list")
	require.Contains(stdout, "No Workloads (Deployments, StatefulSets, or ReplicaSets)")

	stdout = s.TelepresenceConnect(ctx, "--mapped-namespaces", "all")
	require.Empty(stdout)

	stdout = itest.TelepresenceOk(ctx, "list")
	require.NotContains(stdout, "No Workloads (Deployments, StatefulSets, or ReplicaSets)")
}

func (s *multipleServicesSuite) Test_RepeatedConnect() {
	totalErrCount := int64(0)
	for i := 0; i < s.ServiceCount(); i++ {
		url := fmt.Sprintf("http://%s-%d.%s", s.Name(), i, s.AppNamespace())
		for v := 0; v < 30; v++ {
			s.Run(fmt.Sprintf("test-%d", i*30+v), func() {
				ctx := s.Context()
				s.T().Parallel()
				time.Sleep(time.Duration(rand.Intn(100)) * time.Millisecond)
				assert := s.Assert()
				cl := http.Client{}
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				req.Close = true
				assert.NoError(err)
				res, err := cl.Do(req)
				if err != nil {
					if atomic.AddInt64(&totalErrCount, 1) > 2 {
						s.Failf("failed more than 2 times: %v", err.Error())
					}
					return
				}
				assert.Equal(res.StatusCode, http.StatusOK)
				_, err = io.Copy(io.Discard, res.Body)
				assert.NoError(err)
				assert.NoError(res.Body.Close())
			})
		}
	}
}

func (s *multipleServicesSuite) Test_ProxiesOutboundTraffic() {
	ctx := s.Context()
	for i := 0; i < s.ServiceCount(); i++ {
		svc := fmt.Sprintf("%s-%d.%s", s.Name(), i, s.AppNamespace())
		expectedOutput := fmt.Sprintf("Request served by %s-%d", s.Name(), i)
		s.Require().Eventually(
			// condition
			func() bool {
				dlog.Infof(ctx, "trying %q...", "http://"+svc)
				hc := http.Client{Timeout: time.Second}
				resp, err := hc.Get("http://" + svc)
				if err != nil {
					dlog.Error(ctx, err)
					return false
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					dlog.Error(ctx, err)
					return false
				}
				dlog.Infof(ctx, "body: %q", body)
				return strings.Contains(string(body), expectedOutput)
			},
			15*time.Second, // waitfor
			3*time.Second,  // polling interval
			`body of %q contains %q`, "http://"+svc, expectedOutput,
		)
	}
}
