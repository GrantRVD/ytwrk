package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/cenkalti/backoff"
	"github.com/sirupsen/logrus"
	"github.com/terorie/yt-mango/api"
	"github.com/terorie/yt-mango/data"
	"github.com/terorie/yt-mango/net"
	"github.com/valyala/fasthttp"
)

var continuationLimitReached = fmt.Errorf("continuation limit reached")

func (s *Scheduler) startWorker(out chan<- []byte, job *Job) {
	s.jobLock.Lock()
	s.jobs[job] = true
	s.jobLock.Unlock()

	go func() {
		err := streamComments(out, job)
		if err != nil {
			logrus.WithField("video", job.VideoID).WithError(err).
				Error("Failed to stream comments of video")
		}
		s.jobLock.Lock()
		delete(s.jobs, job)
		s.jobLock.Unlock()
	}()
}

func streamComments(out chan<- []byte, job *Job) error {
	videoID, err := api.GetVideoID(job.VideoID)
	if err != nil { return err }

	vid, err := simpleGetVideo(videoID)
	if err != nil { return err }

	cont := api.InitialCommentContinuation(vid)
	if cont == nil {
		return fmt.Errorf("failed to request comments")
	}

	j := videoCommentsJob{
		Job: job,
		log: logrus.WithField("video", job.VideoID),
		comments: make(chan data.Comment),
	}
	var runFunc func()
	switch j.Job.Sort {
	case "top":
		runFunc = func() { j.streamComments(cont) }
	case "age":
		runFunc = func() { j.streamNewComments(cont) }
	case "live":
		runFunc = func() { j.streamLiveComments(cont) }
	default:
		return fmt.Errorf("unknown sort order: %s", j.Sort)
	}
	j.wg.Add(1)
	go func() {
		defer j.wg.Done()
		runFunc()
	}()
	go func() {
		j.wg.Wait()
		close(j.comments)
	}()

	for comment := range j.comments {
		commentBuf, err := json.Marshal(&comment)
		if err != nil { panic(err) }
		out <- commentBuf
	}
	return nil
}

// TODO Copied

type videoCommentsJob struct {
	*Job
	log *logrus.Entry
	wg sync.WaitGroup
	comments chan data.Comment
}

func simpleGetVideo(videoID string) (v *data.Video, err error) {
	videoReq := api.GrabVideo(videoID)

	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(res)

	err = backoff.Retry(func() error {
		err = net.Client.Do(videoReq, res)
		if err == fasthttp.ErrNoFreeConns {
			logrus.WithError(err).Warn("No free conns, throttling")
			return err
		} else if err != nil {
			return backoff.Permanent(err)
		}
		return nil
	}, backoff.NewExponentialBackOff())
	if err != nil {
		return nil, err
	}

	v = new(data.Video)
	v.ID = videoID
	err = api.ParseVideo(v, res)
	if err != nil { return nil, err }

	return
}

func (j *videoCommentsJob) streamComments(cont *api.CommentContinuation) {
	var err error
	for i := 0; true; i++ {
		var page api.CommentPage
		page, err = j.nextCommentPage(cont, i)
		if err != nil {
			break
		}

		for _, comment := range page.Comments {
			subCont := api.CommentRepliesContinuation(&comment, cont)
			if subCont != nil {
				j.wg.Add(1)
				go func() {
					defer j.wg.Done()
					j.streamComments(subCont)
				}()
			}
			j.comments <- comment
		}

		if !page.MoreComments {
			break
		}
	}
	if err == continuationLimitReached {
		j.log.Warn("Continuation limit reached")
	} else if err != nil {
		j.log.WithError(err).Error("Comment stream aborted")
	}
}

func (j *videoCommentsJob) streamNewComments(cont *api.CommentContinuation) {
	var err error
	var page api.CommentPage
	page, err = j.nextCommentPage(cont, -1)
	if err != nil {
		j.log.WithError(err).Error("Comment stream aborted")
		return
	}
	*cont = *page.NewComments

	j.streamComments(cont)
}

func (j *videoCommentsJob) streamLiveComments(cont *api.CommentContinuation) {
	// TODO Basic deduplication

	var err error
	var page api.CommentPage
	page, err = j.nextCommentPage(cont, -1)
	if err != nil {
		j.log.WithError(err).Error("Comment stream aborted")
		return
	}
	*cont = *page.NewComments

	for i := 0; true; i++ {
		page, err = j.nextCommentPage(cont, i)
		if err != nil {
			break
		}
		*cont = *page.NewComments
		for _, comment := range page.Comments {
			subCont := api.CommentRepliesContinuation(&comment, cont)
			if subCont != nil {
				j.wg.Add(1)
				go func() {
					defer j.wg.Done()
					j.streamComments(subCont)
				}()
			}
			j.comments <- comment
		}
	}
	if err != nil {
		j.log.WithError(err).Error("Comment stream aborted")
	}
}

func (j *videoCommentsJob) nextCommentPage(cont *api.CommentContinuation, i int) (page api.CommentPage, err error) {
	req := api.GrabCommentPage(cont)
	defer fasthttp.ReleaseRequest(req)
	res := fasthttp.AcquireResponse()
	err = backoff.Retry(func() error {
		err = net.Client.Do(req, res)
		if err == fasthttp.ErrNoFreeConns {
			logrus.WithError(err).Warn("No free conns, throttling")
			return err
		} else if err != nil {
			return backoff.Permanent(err)
		}
		return nil
	}, backoff.NewExponentialBackOff())
	if err != nil {
		return page, err
	}
	switch res.StatusCode() {
	case fasthttp.StatusRequestEntityTooLarge,
		fasthttp.StatusRequestURITooLong:
		return page, continuationLimitReached
	}

	page, err = api.ParseCommentsPage(res, cont)
	if err != nil { return page, err }
	for _, cErr := range page.CommentParseErrs {
		j.log.WithError(cErr).Error("Failed to parse comment")
	}

	if cont.ParentID == "" {
		j.log.WithFields(logrus.Fields{
			"video_id": j.VideoID,
			"index": i,
		}).Info("Page")
	} else {
		j.log.WithFields(logrus.Fields{
			"video_id": j.VideoID,
			"index": i,
			"parent_id": cont.ParentID,
		}).Info("Sub page")
	}
	atomic.AddInt64(&j.Pages, 1)
	atomic.AddInt64(&j.Items, int64(len(page.Comments)))
	return page, nil
}

