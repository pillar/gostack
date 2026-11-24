package social

import (
	"net/http"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
)

// friendBoard ranks the caller and their friends on a configured board for the
// current season, using only accepted scores. Friends who disabled
// show_on_friend_boards are left out; the caller always sees their own score.
func (m *Module) friendBoard(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	gid, pid := m.ident(r)
	board := r.PathValue("board")
	if !game.ValidID(board) {
		return httpx.NotFound("board not found")
	}
	doc, _, err := m.Kit.CurrentDocument(ctx, m.DB.Read, gid)
	if err != nil {
		return err
	}
	def, ok := doc.Boards[board]
	if !ok {
		return httpx.NotFound("board not found")
	}
	season := r.URL.Query().Get("season")
	if season == "" {
		season = game.SeasonID(def.Period, m.Now())
	} else if len(season) > 16 {
		return httpx.BadRequest("invalid season")
	}
	dir := "DESC"
	if def.Order == "asc" {
		dir = "ASC"
	}
	rows, err := m.DB.Read.QueryContext(ctx, `SELECT s.player_id, s.score, s.achieved_at, coalesce(p.display_name,'')
		FROM scores s LEFT JOIN social_profiles p ON p.game_id=s.game_id AND p.player_id=s.player_id
		WHERE s.game_id=? AND s.board_id=? AND s.season_id=? AND s.status='accepted'
		  AND s.player_id IN (SELECT ? UNION SELECT friend_id FROM friendships WHERE game_id=? AND player_id=?)
		  AND (s.player_id=? OR coalesce(p.show_on_friend_boards,1)=1)
		ORDER BY s.score `+dir+`, s.achieved_at ASC, s.player_id ASC`,
		gid, board, season, pid, gid, pid, pid)
	if err != nil {
		return err
	}
	defer rows.Close()
	type entry struct {
		Rank        int    `json:"rank"`
		PlayerID    string `json:"player_id"`
		DisplayName string `json:"display_name"`
		Score       int64  `json:"score"`
		AchievedAt  int64  `json:"achieved_at"`
		Me          bool   `json:"me"`
	}
	entries := []entry{}
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.PlayerID, &e.Score, &e.AchievedAt, &e.DisplayName); err != nil {
			return err
		}
		e.Rank, e.Me = len(entries)+1, e.PlayerID == pid
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"board": board, "season": season, "entries": entries})
	return nil
}
