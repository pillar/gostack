package game

// OfferRawForTest enqueues an event row bypassing ingress validation, as a
// poison row that slipped past it would arrive at the flusher.
func (a *Analytics) OfferRawForTest(gameID, playerID, eventID, platform string, now int64) bool {
	return a.offer(analyticsRow{GameID: gameID, PlayerID: playerID, EventID: eventID, SessionID: "s", Name: "poison",
		Platform: platform, ClientVersion: "1", SchemaVersion: 1, ClientTime: now, ServerTime: now, Properties: []byte("{}")})
}
