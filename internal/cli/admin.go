package cli

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"strings"
)

func cmdAdmin(ctx context.Context, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(Err, "usage: dkm admin <subject> <action> [flags]")
		fmt.Fprintln(Err, "\n  dkm admin team create --id acme --name \"Acme Engineering\"")
		fmt.Fprintln(Err, "  dkm admin user create --id kuong --name Kuong [--admin]")
		fmt.Fprintln(Err, "  dkm admin user list")
		fmt.Fprintln(Err, "  dkm admin key issue --user kuong --label laptop")
		fmt.Fprintln(Err, "  dkm admin key list")
		fmt.Fprintln(Err, "  dkm admin key revoke <key-id>")
		fmt.Fprintln(Err, "  dkm admin audit [--user u] [--action a] [--since 2026-08-01T00:00:00Z]")
		fmt.Fprintln(Err, "  dkm admin runs")
		fmt.Fprintln(Err, "\nThese need an admin key. `dkm login` with the key printed on first boot.")
		return 2
	}

	subject, action, rest := args[0], args[1], args[2:]

	switch subject + " " + action {
	case "team create":
		return adminTeamCreate(ctx, rest)
	case "user create":
		return adminUserCreate(ctx, rest)
	case "user list":
		return adminUserList(ctx)
	case "key issue":
		return adminKeyIssue(ctx, rest)
	case "key list":
		return adminKeyList(ctx)
	case "key revoke":
		return adminKeyRevoke(ctx, rest)
	default:
		// `dkm admin audit` and `dkm admin runs` take no second word.
		switch subject {
		case "audit":
			return adminAudit(ctx, args[1:])
		case "runs":
			return adminRuns(ctx)
		}
		return fail("unknown admin command %q", subject+" "+action)
	}
}

func adminTeamCreate(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("team create", flag.ContinueOnError)
	fs.SetOutput(Err)
	id := fs.String("id", "", "team id, e.g. acme")
	name := fs.String("name", "", "display name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *id == "" {
		return fail("--id is required")
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}
	var out map[string]any
	if err := c.Admin(ctx, http.MethodPost, "/v1/admin/teams",
		map[string]any{"id": *id, "name": *name}, &out); err != nil {
		return failErr(err)
	}
	fmt.Fprintf(Out, "Created team %s\n", *id)
	return 0
}

func adminUserCreate(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	fs.SetOutput(Err)
	id := fs.String("id", "", "user id, e.g. kuong")
	team := fs.String("team", "", "team id; the caller's team by default")
	name := fs.String("name", "", "display name")
	admin := fs.Bool("admin", false, "grant admin rights")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *id == "" {
		return fail("--id is required")
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}
	var out map[string]any
	if err := c.Admin(ctx, http.MethodPost, "/v1/admin/users", map[string]any{
		"id": *id, "team_id": *team, "name": *name, "is_admin": *admin,
	}, &out); err != nil {
		return failErr(err)
	}
	fmt.Fprintf(Out, "Created user %s in team %s\n", *id, orDash(fmt.Sprint(out["team_id"])))
	return 0
}

func adminUserList(ctx context.Context) int {
	c, err := newClient()
	if err != nil {
		return failErr(err)
	}
	var out struct {
		Users []struct {
			ID      string `json:"id"`
			TeamID  string `json:"team_id"`
			Name    string `json:"name"`
			IsAdmin bool   `json:"is_admin"`
		} `json:"users"`
	}
	if err := c.Admin(ctx, http.MethodGet, "/v1/admin/users", nil, &out); err != nil {
		return failErr(err)
	}
	if len(out.Users) == 0 {
		fmt.Fprintln(Out, "No users.")
		return 0
	}
	rows := [][]string{{"ID", "NAME", "TEAM", "ADMIN"}}
	for _, u := range out.Users {
		rows = append(rows, []string{u.ID, u.Name, u.TeamID, yesNo(u.IsAdmin)})
	}
	table(Out, rows)
	return 0
}

func adminKeyIssue(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("key issue", flag.ContinueOnError)
	fs.SetOutput(Err)
	user := fs.String("user", "", "user id the key belongs to")
	label := fs.String("label", "", "what this key is for, e.g. laptop")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *user == "" {
		return fail("--user is required")
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}
	var out struct {
		ID     string `json:"id"`
		Prefix string `json:"prefix"`
		Key    string `json:"key"`
	}
	if err := c.Admin(ctx, http.MethodPost, "/v1/admin/keys",
		map[string]any{"user_id": *user, "label": *label}, &out); err != nil {
		return failErr(err)
	}

	fmt.Fprintf(Out, "%s\n", strings.Repeat("─", 68))
	fmt.Fprintf(Out, "  %s\n", out.Key)
	fmt.Fprintf(Out, "%s\n", strings.Repeat("─", 68))
	fmt.Fprintf(Out, "  key id  %s\n", out.ID)
	fmt.Fprintf(Out, "  user    %s\n\n", *user)
	// Said plainly, because the alternative is a support conversation that
	// starts "I closed the terminal".
	fmt.Fprintf(Out, "  Shown once. It is stored hashed and cannot be recovered.\n")
	fmt.Fprintf(Out, "  Give it to one person for one machine — revoking then affects nobody else.\n")
	return 0
}

func adminKeyList(ctx context.Context) int {
	c, err := newClient()
	if err != nil {
		return failErr(err)
	}
	var out struct {
		Keys []struct {
			ID         string  `json:"id"`
			UserID     string  `json:"user_id"`
			Prefix     string  `json:"prefix"`
			Label      string  `json:"label"`
			LastUsedAt *string `json:"last_used_at"`
			RevokedAt  *string `json:"revoked_at"`
		} `json:"keys"`
	}
	if err := c.Admin(ctx, http.MethodGet, "/v1/admin/keys", nil, &out); err != nil {
		return failErr(err)
	}
	if len(out.Keys) == 0 {
		fmt.Fprintln(Out, "No keys.")
		return 0
	}

	rows := [][]string{{"KEY ID", "USER", "PREFIX", "LABEL", "LAST USED", "STATE"}}
	for _, k := range out.Keys {
		state := "active"
		if k.RevokedAt != nil {
			state = "revoked"
		}
		last := "never"
		if k.LastUsedAt != nil {
			last = (*k.LastUsedAt)[:10]
		}
		rows = append(rows, []string{k.ID, k.UserID, k.Prefix + "…", orDash(k.Label), last, state})
	}
	table(Out, rows)
	return 0
}

func adminKeyRevoke(ctx context.Context, args []string) int {
	if len(args) < 1 {
		return fail("usage: dkm admin key revoke <key-id>")
	}
	c, err := newClient()
	if err != nil {
		return failErr(err)
	}
	if err := c.Admin(ctx, http.MethodDelete, "/v1/admin/keys/"+args[0], nil, nil); err != nil {
		return failErr(err)
	}
	fmt.Fprintf(Out, "Revoked %s. The next request using it is rejected; there is no cache to wait out.\n", args[0])
	return 0
}

func adminAudit(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(Err)
	user := fs.String("user", "", "filter by user id")
	action := fs.String("action", "", "filter by action, e.g. memory.delete")
	since := fs.String("since", "", "RFC3339 timestamp")
	limit := fs.Int("limit", 50, "maximum entries")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	c, err := newClient()
	if err != nil {
		return failErr(err)
	}

	query := []string{}
	add := func(k, v string) {
		if v != "" {
			query = append(query, k+"="+v)
		}
	}
	add("user", *user)
	add("action", *action)
	add("since", *since)
	add("limit", fmt.Sprint(*limit))

	var out struct {
		Entries []struct {
			CreatedAt string `json:"created_at"`
			UserID    string `json:"user_id"`
			Action    string `json:"action"`
			Target    string `json:"target"`
			IP        string `json:"ip"`
		} `json:"entries"`
	}
	if err := c.Admin(ctx, http.MethodGet, "/v1/admin/audit?"+strings.Join(query, "&"), nil, &out); err != nil {
		return failErr(err)
	}
	if len(out.Entries) == 0 {
		fmt.Fprintln(Out, "No audit entries match.")
		return 0
	}

	rows := [][]string{{"WHEN", "WHO", "ACTION", "TARGET", "IP"}}
	for _, e := range out.Entries {
		when := e.CreatedAt
		if len(when) > 19 {
			when = when[:19]
		}
		rows = append(rows, []string{when, orDash(e.UserID), e.Action, truncate(orDash(e.Target), 30), orDash(e.IP)})
	}
	table(Out, rows)
	return 0
}

func adminRuns(ctx context.Context) int {
	c, err := newClient()
	if err != nil {
		return failErr(err)
	}
	var out struct {
		Runs []struct {
			Tier         int     `json:"tier"`
			Project      string  `json:"project"`
			StartedAt    string  `json:"started_at"`
			Items        int     `json:"items"`
			Produced     int     `json:"produced"`
			Deduped      int     `json:"deduped"`
			InputTokens  int     `json:"input_tokens"`
			OutputTokens int     `json:"output_tokens"`
			Error        *string `json:"error"`
		} `json:"runs"`
		Spend struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"spend_last_30d"`
	}
	if err := c.Admin(ctx, http.MethodGet, "/v1/admin/runs", nil, &out); err != nil {
		return failErr(err)
	}

	if len(out.Runs) == 0 {
		fmt.Fprintln(Out, "No consolidation runs yet.")
	} else {
		rows := [][]string{{"WHEN", "TIER", "PROJECT", "IN", "OUT", "NEW", "DEDUP", "TOKENS"}}
		for _, r := range out.Runs {
			when := r.StartedAt
			if len(when) > 16 {
				when = when[:16]
			}
			state := fmt.Sprint(r.Items)
			if r.Error != nil {
				state = "error"
			}
			rows = append(rows, []string{
				when, fmt.Sprint(r.Tier), truncate(orDash(r.Project), 34),
				state, fmt.Sprint(r.Produced), fmt.Sprint(r.Produced), fmt.Sprint(r.Deduped),
				fmt.Sprint(r.InputTokens + r.OutputTokens),
			})
		}
		table(Out, rows)
	}

	// Printed whether or not there are runs: the number people want is what
	// this has cost, and a zero is an answer too.
	fmt.Fprintf(Out, "\nLast 30 days: %d input tokens, %d output tokens.\n",
		out.Spend.InputTokens, out.Spend.OutputTokens)
	return 0
}
