package line

import "os"

const (
	maxTextLength    = 5000
	maxReplyMessages = 5
	replyTokenTTL    = 25 // seconds, buffer before 30s expiry
	loadingSeconds   = 60
	loadingAPIURL    = "https://api.line.me/v2/bot/chat/loading/start"

	// DEPRECATED (goclaw-line-channel-extract-esmith phase 3): transitional
	// script path still referenced by conversation.go. Removed in phase 4
	// when conversation.go moves to plugins/esmith-km.
	meetingsPipelineScript = "/home/ubuntu/odoo_dev/esmith-specs/scripts/km-meeting-pipeline.sh"
)

// getMeetingPipelineScript returns the absolute path of km-meeting-pipeline.sh,
// honoring the KM_MEETING_PIPELINE_SCRIPT env var override.
//
// DEPRECATED (goclaw-line-channel-extract-esmith phase 3): transitional
// helper kept for conversation.go during the multi-phase move. Removed
// in phase 4 when conversation.go moves to plugins/esmith-km.
func getMeetingPipelineScript() string {
	if v := os.Getenv("KM_MEETING_PIPELINE_SCRIPT"); v != "" {
		return v
	}
	return meetingsPipelineScript
}
