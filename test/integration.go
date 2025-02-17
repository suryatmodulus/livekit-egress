//go:build integration

package test

import (
	"context"
	"fmt"
	"io/fs"
	"math/rand"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/livekit/egress/pkg/config"
	"github.com/livekit/egress/pkg/service"
	"github.com/livekit/egress/pkg/types"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"
	lksdk "github.com/livekit/server-sdk-go"
)

const (
	streamUrl1     = "rtmp://localhost:1935/live/stream1"
	redactedUrl1   = "rtmp://localhost:1935/live/*******"
	streamUrl2     = "rtmp://localhost:1935/live/stream10"
	redactedUrl2   = "rtmp://localhost:1935/live/********"
	badStreamUrl   = "rtmp://sfo.contribute.live-video.net/app/fake1"
	redactedBadUrl = "rtmp://sfo.contribute.live-video.net/app/*****"
	webUrl         = "https://www.youtube.com/watch?v=wjQq0nSGS28&t=5205s"
)

type testCase struct {
	name           string
	audioOnly      bool
	videoOnly      bool
	filename       string
	sessionTimeout time.Duration

	// used by room and track composite tests
	fileType livekit.EncodedFileType
	options  *livekit.EncodingOptions
	preset   livekit.EncodingOptionsPreset

	// used by segmented file tests
	playlist       string
	filenameSuffix livekit.SegmentedFileSuffix

	// used by track and track composite tests
	audioCodec types.MimeType
	videoCodec types.MimeType

	// used by track tests
	outputType types.OutputType

	expectVideoTranscoding bool
}

func RunTestSuite(t *testing.T, conf *TestConfig, bus psrpc.MessageBus, templateFs fs.FS) {
	lksdk.SetLogger(logger.LogRLogger(logr.Discard()))

	// connect to room
	room, err := lksdk.ConnectToRoom(conf.WsUrl, lksdk.ConnectInfo{
		APIKey:              conf.ApiKey,
		APISecret:           conf.ApiSecret,
		RoomName:            conf.RoomName,
		ParticipantName:     "egress-sample",
		ParticipantIdentity: fmt.Sprintf("sample-%d", rand.Intn(100)),
	}, lksdk.NewRoomCallback())
	require.NoError(t, err)
	defer room.Disconnect()

	// start service
	ioClient, err := rpc.NewIOInfoClient("test_io_client", bus)
	require.NoError(t, err)
	svc, err := service.NewService(conf.ServiceConfig, bus, nil, ioClient)
	require.NoError(t, err)

	psrpcClient, err := rpc.NewEgressClient(livekit.NodeID(utils.NewGuid("TEST_")), bus)
	require.NoError(t, err)

	// start debug handler
	svc.StartDebugHandlers()

	// start templates handler
	err = svc.StartTemplatesServer(templateFs)
	require.NoError(t, err)

	go func() {
		err := svc.Run()
		require.NoError(t, err)
	}()
	t.Cleanup(func() { svc.Stop(true) })
	time.Sleep(time.Second * 3)

	// subscribe to update channel
	psrpcUpdates := make(chan *livekit.EgressInfo, 100)
	_, err = newIOTestServer(bus, psrpcUpdates)
	require.NoError(t, err)

	// update test config
	conf.svc = svc
	conf.client = psrpcClient
	conf.updates = psrpcUpdates
	conf.room = room

	// check status
	if conf.HealthPort != 0 {
		status := getStatus(t, svc)
		require.Len(t, status, 1)
		require.Contains(t, status, "CpuLoad")
	}

	// run tests
	runRoomCompositeTests(t, conf)
	runWebTests(t, conf)
	runTrackCompositeTests(t, conf)
	runTrackTests(t, conf)
}

func runWebTest(t *testing.T, conf *TestConfig, name string, audioCodec, videoCodec types.MimeType, f func(t *testing.T)) {
	t.Run(name, func(t *testing.T) {
		awaitIdle(t, conf.svc)
		publishSamplesToRoom(t, conf.room, audioCodec, videoCodec, conf.Muting)
		f(t)
	})
}

func runSDKTest(t *testing.T, conf *TestConfig, name string, audioCodec, videoCodec types.MimeType,
	f func(t *testing.T, audioTrackID, videoTrackID string),
) {
	t.Run(name, func(t *testing.T) {
		awaitIdle(t, conf.svc)
		audioTrackID, videoTrackID := publishSamplesToRoom(t, conf.room, audioCodec, videoCodec, conf.Muting)
		f(t, audioTrackID, videoTrackID)
	})
}

func awaitIdle(t *testing.T, svc *service.Service) {
	svc.KillAll()
	for i := 0; i < 30; i++ {
		status := getStatus(t, svc)
		if len(status) == 1 {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("service not idle after 30s")
}

func runFileTest(t *testing.T, conf *TestConfig, req *rpc.StartEgressRequest, test *testCase) {
	conf.SessionLimits.FileOutputMaxDuration = test.sessionTimeout

	// start
	egressID := startEgress(t, conf, req)

	var res *livekit.EgressInfo
	if conf.SessionLimits.FileOutputMaxDuration > 0 {
		time.Sleep(conf.SessionLimits.FileOutputMaxDuration + time.Second)

		res = checkStoppedEgress(t, conf, egressID, livekit.EgressStatus_EGRESS_LIMIT_REACHED)
	} else {
		time.Sleep(time.Second * 25)

		// stop
		res = stopEgress(t, conf, egressID)
	}

	// get params
	p, err := config.GetValidatedPipelineConfig(conf.ServiceConfig, req)
	require.NoError(t, err)
	if p.Outputs[types.EgressTypeFile].OutputType == types.OutputTypeUnknownFile {
		p.Outputs[types.EgressTypeFile].OutputType = test.outputType
	}

	require.Equal(t, test.expectVideoTranscoding, p.VideoTranscoding)

	// verify
	verifyFile(t, conf, p, res)
}

func runStreamTest(t *testing.T, conf *TestConfig, req *rpc.StartEgressRequest, test *testCase) {
	conf.SessionLimits.StreamOutputMaxDuration = test.sessionTimeout

	if conf.SessionLimits.StreamOutputMaxDuration > 0 {
		runTimeLimitStreamTest(t, conf, req, test)
	} else {
		runMultipleStreamTest(t, conf, req, test)
	}
}

func runTimeLimitStreamTest(t *testing.T, conf *TestConfig, req *rpc.StartEgressRequest, test *testCase) {
	egressID := startEgress(t, conf, req)

	time.Sleep(time.Second * 5)

	// get params
	p, err := config.GetValidatedPipelineConfig(conf.ServiceConfig, req)
	require.NoError(t, err)

	require.Equal(t, test.expectVideoTranscoding, p.VideoTranscoding)

	verifyStreams(t, p, conf, streamUrl1)

	time.Sleep(conf.SessionLimits.StreamOutputMaxDuration - time.Second*4)

	checkStoppedEgress(t, conf, egressID, livekit.EgressStatus_EGRESS_LIMIT_REACHED)
}

func runMultipleStreamTest(t *testing.T, conf *TestConfig, req *rpc.StartEgressRequest, test *testCase) {
	ctx := context.Background()
	egressID := startEgress(t, conf, req)

	time.Sleep(time.Second * 5)

	// get params
	p, err := config.GetValidatedPipelineConfig(conf.ServiceConfig, req)
	require.NoError(t, err)

	// verify stream
	require.Equal(t, test.expectVideoTranscoding, p.VideoTranscoding)
	verifyStreams(t, p, conf, streamUrl1)

	// add one good stream url and a couple bad ones
	_, err = conf.client.UpdateStream(ctx, egressID, &livekit.UpdateStreamRequest{
		EgressId:      egressID,
		AddOutputUrls: []string{badStreamUrl, streamUrl2},
	})
	require.NoError(t, err)

	time.Sleep(time.Second * 5)

	update := getUpdate(t, conf, egressID)
	require.Equal(t, livekit.EgressStatus_EGRESS_ACTIVE.String(), update.Status.String())
	require.Len(t, update.StreamResults, 3)
	for _, info := range update.StreamResults {
		switch info.Url {
		case redactedUrl1, redactedUrl2:
			require.Equal(t, livekit.StreamInfo_ACTIVE.String(), info.Status.String())

		case redactedBadUrl:
			require.Equal(t, livekit.StreamInfo_FAILED.String(), info.Status.String())

		default:
			t.Fatal("invalid stream url in result")
		}
	}

	require.Equal(t, test.expectVideoTranscoding, p.VideoTranscoding)

	// verify the good stream urls
	verifyStreams(t, p, conf, streamUrl1, streamUrl2)

	// remove one of the stream urls
	_, err = conf.client.UpdateStream(ctx, egressID, &livekit.UpdateStreamRequest{
		EgressId:         egressID,
		RemoveOutputUrls: []string{streamUrl1},
	})
	require.NoError(t, err)

	time.Sleep(time.Second * 5)

	// verify the remaining stream
	verifyStreams(t, p, conf, streamUrl2)

	time.Sleep(time.Second * 10)

	// stop
	res := stopEgress(t, conf, egressID)

	// verify egress info
	require.Empty(t, res.Error)
	require.NotZero(t, res.StartedAt)
	require.NotZero(t, res.EndedAt)

	// check stream info
	require.Len(t, res.StreamResults, 3)
	for _, info := range res.StreamResults {
		require.NotZero(t, info.StartedAt)
		require.NotZero(t, info.EndedAt)

		switch info.Url {
		case redactedUrl1:
			require.Equal(t, livekit.StreamInfo_FINISHED.String(), info.Status.String())
			require.Greater(t, float64(info.Duration)/1e9, 15.0)

		case redactedUrl2:
			require.Equal(t, livekit.StreamInfo_FINISHED.String(), info.Status.String())
			require.Greater(t, float64(info.Duration)/1e9, 10.0)

		case redactedBadUrl:
			require.Equal(t, livekit.StreamInfo_FAILED.String(), info.Status.String())

		default:
			t.Fatal("invalid stream url in result")
		}
	}
}

func runSegmentsTest(t *testing.T, conf *TestConfig, req *rpc.StartEgressRequest, test *testCase) {
	conf.SessionLimits.SegmentOutputMaxDuration = test.sessionTimeout

	egressID := startEgress(t, conf, req)

	var res *livekit.EgressInfo
	if conf.SessionLimits.SegmentOutputMaxDuration > 0 {
		time.Sleep(conf.SessionLimits.SegmentOutputMaxDuration + time.Second)

		res = checkStoppedEgress(t, conf, egressID, livekit.EgressStatus_EGRESS_LIMIT_REACHED)
	} else {
		time.Sleep(time.Second * 25)

		// stop
		res = stopEgress(t, conf, egressID)
	}

	// get params
	p, err := config.GetValidatedPipelineConfig(conf.ServiceConfig, req)
	require.NoError(t, err)

	require.Equal(t, test.expectVideoTranscoding, p.VideoTranscoding)
	verifySegments(t, conf, p, test.filenameSuffix, res)
}

func runMultipleTest(
	t *testing.T,
	conf *TestConfig,
	req *rpc.StartEgressRequest,
	file, stream, segments bool,
	filenameSuffix livekit.SegmentedFileSuffix,
) {
	conf.SessionLimits = config.SessionLimits{}

	egressID := startEgress(t, conf, req)
	time.Sleep(time.Second * 10)

	// get params
	p, err := config.GetValidatedPipelineConfig(conf.ServiceConfig, req)
	require.NoError(t, err)

	if stream {
		_, err := conf.client.UpdateStream(context.Background(), egressID, &livekit.UpdateStreamRequest{
			EgressId:      egressID,
			AddOutputUrls: []string{streamUrl1},
		})
		require.NoError(t, err)

		time.Sleep(time.Second * 10)
		verifyStreams(t, p, conf, streamUrl1)
		time.Sleep(time.Second * 10)
	} else {
		time.Sleep(time.Second * 20)
	}

	res := stopEgress(t, conf, egressID)
	if file {
		verifyFile(t, conf, p, res)
	}
	if segments {
		verifySegments(t, conf, p, filenameSuffix, res)
	}
}
