package esmithkm

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/channels/line"
)

// parseBaseURL extracts the scheme+host origin from a URL, discarding any
// path / query / fragment. Used by buildOdooDeepLink to derive the Odoo
// base URL from the MCP endpoint (which points at `/mcp/v1/message` etc).
// Returns empty string on parse failure.
func parseBaseURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// project is a tiny view of project.project for the picker.
// project represents a pickable project in the picker bubble. ID is
// always the project.project.id (NOT job.project.id) — the LIFF primary
// path returns job.project records keyed by job.project.id, which we
// immediately resolve to project.project.id via the job.project.project_id
// m2o relation so downstream code (finalize → job.meeting.minutes.project_id
// FK → project.project) never sees the wrong key. IsFavorite mirrors the
// FR module's "我的專案" favorites flag so the picker bubble can prefix a
// star marker.
type project struct {
	ID         int
	Name       string
	IsFavorite bool
}

// partner is a tiny view of res.partner for the attendees picker.
type partner struct {
	ID   int
	Name string
}

// meetingMinutesActionXMLID is the Odoo action XML id used to build deep
// links to job.meeting.minutes records. This whole flow is anchored on
// one model; if other models ever need links, pass an action id instead.
const meetingMinutesActionXMLID = "job_working_plan.action_job_meeting_minutes"

// buildPostbackData encodes the canonical postback payload used by every
// picker bubble. The format is a URL-encoded query string so handlePostback
// can route on `action` regardless of which bubble emitted the event.
//
//	action=update&ref=<ref>&field=<field>&value=<value>
//	action=finalize&ref=<ref>
//	action=cancel&ref=<ref>
//	action=toggle&ref=<ref>&value=<partner_id>
//	action=submit_attendees&ref=<ref>
func buildPostbackData(values map[string]string) string {
	v := url.Values{}
	for k, val := range values {
		v.Set(k, val)
	}
	return v.Encode()
}

// buildProjectPicker renders the project_picker bubble. The caller is
// responsible for enforcing the picker size cap — this builder trusts
// whatever slice it receives. Upstream callers use maxProjectsPerPicker
// as the hard limit.
func buildProjectPicker(ref, subject string, projects []project) ([]byte, error) {
	if len(projects) == 0 {
		return nil, fmt.Errorf("no projects to show")
	}

	header := line.FlexTextBox(
		"📝 "+line.Truncate(subject, 60),
		"size", "md",
		"weight", "bold",
		"wrap", true,
	)
	subheader := line.FlexTextBox(
		"請選擇此次會議所屬專案：",
		"size", "sm",
		"color", "#868e96",
		"wrap", true,
	)

	body := []map[string]any{header, subheader, line.FlexSeparator(8)}

	for _, p := range projects {
		label := line.Truncate(p.Name, 36)
		if p.IsFavorite {
			// Prefix with star so favorites are visually distinct,
			// matching the FR "我的專案" SPA screen's visual language.
			label = "⭐ " + label
		}
		body = append(body, line.FlexButton(
			label,
			buildPostbackData(map[string]string{
				"action": "update",
				"ref":    ref,
				"field":  "project_id",
				"value":  strconv.Itoa(p.ID),
			}),
		))
	}

	bubble := map[string]any{
		"type": "bubble",
		"size": "mega",
		"body": map[string]any{
			"type":     "box",
			"layout":   "vertical",
			"spacing":  "sm",
			"contents": body,
		},
	}
	return json.Marshal(bubble)
}

// buildLocationPicker renders the location_picker bubble — 4 fixed
// quick-reply style buttons.
func buildLocationPicker(ref string) ([]byte, error) {
	options := []string{"線上", "工地會議室", "辦公室", "其他"}
	body := []map[string]any{
		line.FlexTextBox(
			"📍 會議地點？",
			"size", "md",
			"weight", "bold",
		),
		line.FlexSeparator(8),
	}
	for _, opt := range options {
		body = append(body, line.FlexButton(
			opt,
			buildPostbackData(map[string]string{
				"action": "update",
				"ref":    ref,
				"field":  "meeting_location",
				"value":  opt,
			}),
		))
	}
	bubble := map[string]any{
		"type": "bubble",
		"body": map[string]any{
			"type":     "box",
			"layout":   "vertical",
			"spacing":  "sm",
			"contents": body,
		},
	}
	return json.Marshal(bubble)
}

// buildAttendeesPicker renders a bubble with toggleable partner buttons.
// Selected partners are visually distinguished (✓ prefix) and remembered in
// goclaw memory until the user taps the "完成" submit button.
//
// The caller is responsible for enforcing the picker size cap — this
// builder trusts whatever slice it receives. Upstream callers use
// maxAttendeesPerPicker as the hard limit.
func buildAttendeesPicker(ref string, partners []partner, selected map[int]bool) ([]byte, error) {
	body := []map[string]any{
		line.FlexTextBox(
			"👥 出席者（可複選）",
			"size", "md",
			"weight", "bold",
		),
		line.FlexTextBox(
			fmt.Sprintf("已選 %d 人", line.CountSelected(selected)),
			"size", "sm",
			"color", "#868e96",
		),
		line.FlexSeparator(8),
	}
	for _, p := range partners {
		label := line.Truncate(p.Name, 36)
		style := "secondary"
		if selected[p.ID] {
			label = "✓ " + label
			style = "primary"
		}
		btn := line.FlexButton(
			label,
			buildPostbackData(map[string]string{
				"action": "toggle",
				"ref":    ref,
				"value":  strconv.Itoa(p.ID),
			}),
		)
		btn["style"] = style
		body = append(body, btn)
	}

	body = append(body, line.FlexSeparator(8))
	doneBtn := line.FlexButton(
		"✅ 完成選擇",
		buildPostbackData(map[string]string{
			"action": "submit_attendees",
			"ref":    ref,
		}),
	)
	doneBtn["style"] = "primary"
	doneBtn["color"] = "#51cf66"
	body = append(body, doneBtn)

	bubble := map[string]any{
		"type": "bubble",
		"size": "mega",
		"body": map[string]any{
			"type":     "box",
			"layout":   "vertical",
			"spacing":  "sm",
			"contents": body,
		},
	}
	return json.Marshal(bubble)
}

// buildConfirmBubble renders the final summary + Yes/No bubble.
func buildConfirmBubble(ref, subject, projectName, location string, attendeeCount int) ([]byte, error) {
	rows := []map[string]any{
		line.FlexTextBox("📋 會議記錄確認", "size", "md", "weight", "bold"),
		line.FlexSeparator(8),
		line.FlexKVRow("主題", line.Truncate(subject, 60)),
		line.FlexKVRow("專案", line.Truncate(projectName, 40)),
		line.FlexKVRow("地點", location),
		line.FlexKVRow("出席", fmt.Sprintf("%d 人", attendeeCount)),
		line.FlexSeparator(8),
	}

	yes := line.FlexButton("✅ 建立會議記錄", buildPostbackData(map[string]string{
		"action": "finalize",
		"ref":    ref,
	}))
	yes["style"] = "primary"
	yes["color"] = "#51cf66"

	no := line.FlexButton("❌ 取消", buildPostbackData(map[string]string{
		"action": "cancel",
		"ref":    ref,
	}))
	no["style"] = "secondary"

	rows = append(rows, yes, no)

	bubble := map[string]any{
		"type": "bubble",
		"body": map[string]any{
			"type":     "box",
			"layout":   "vertical",
			"spacing":  "sm",
			"contents": rows,
		},
	}
	return json.Marshal(bubble)
}

// buildOdooDeepLink returns a clickable Odoo 18 SPA URL to a record.
// Odoo 18 dropped the legacy `/web#id=...` hash route in the new SPA
// shell — `/odoo/action-<xml_id>/<id>` is the working format. The `model`
// arg is currently ignored (everything goes through meetingMinutesActionXMLID)
// but kept for API compatibility with the previous direct call site.
//
// Resolution order for the base URL:
//  1. h.cfg.OdooBaseURL (explicit)
//  2. scheme+host of h.cfg.MCPURL (via url.Parse)
//
// The previous TrimSuffix("/mcp/v1") approach was broken for MCP URLs
// that ended with `/mcp/v1/message` (which stage35's MCP endpoint does
// as of 2026-04) — the suffix never matched and the full URL including
// `/mcp/v1/message` became the deep link base, producing 404s like
// `https://.../mcp/v1/message/odoo/action-.../494`.
//
// Empty string when the base URL cannot be determined.
func (h *Hook) buildOdooDeepLink(_ string, id int) string {
	base := h.cfg.OdooBaseURL
	if base == "" {
		base = parseBaseURL(h.cfg.MCPURL)
	}
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/odoo/action-%s/%d",
		strings.TrimRight(base, "/"), meetingMinutesActionXMLID, id)
}
