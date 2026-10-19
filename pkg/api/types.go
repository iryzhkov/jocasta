package api

const Version = "jocasta/v1"

type Document struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	Revision    int64  `json:"revision"`
	Content     string `json:"content"`
	Title       string `json:"title"`
	Kind        string `json:"kind"`
	Archived    bool   `json:"archived"`
	UpdatedAt   string `json:"updated_at"`
	Actor       string `json:"actor"`
	ContentHash string `json:"content_hash"`
	Message     string `json:"message,omitempty"`
}
type Mutation struct {
	Operation      string  `json:"operation"`
	Path           string  `json:"path,omitempty"`
	ID             string  `json:"id,omitempty"`
	IfRevision     int64   `json:"if_revision,omitempty"`
	SourceRevision int64   `json:"source_revision,omitempty"`
	Destination    string  `json:"destination,omitempty"`
	Content        *string `json:"content,omitempty"`
	Title          *string `json:"title,omitempty"`
	Kind           *string `json:"kind,omitempty"`
	Message        string  `json:"message,omitempty"`
}
type Receipt struct {
	Version            string   `json:"version"`
	Document           Document `json:"document"`
	RequestedReference string   `json:"requested_reference,omitempty"`
	Alias              bool     `json:"alias"`
	Partial            bool     `json:"partial"`
	FullDocumentHash   string   `json:"full_document_hash"`
	TotalLines         int      `json:"total_lines"`
	RequestedLines     string   `json:"requested_lines,omitempty"`
	ReturnedLines      string   `json:"returned_lines,omitempty"`
}
type Page struct {
	Version        string     `json:"version"`
	Documents      []Document `json:"documents"`
	NextCursor     string     `json:"next_cursor,omitempty"`
	HasMore        bool       `json:"has_more"`
	Complete       bool       `json:"complete"`
	ScannedCount   int        `json:"scanned_count"`
	CandidateLimit int        `json:"candidate_limit,omitempty"`
	// Ranking names the path that ordered a search page: "bm25" when the
	// full-text index scored it, "substring" when search fell back to name
	// matching. RankingReason says why the fallback happened. Both are empty
	// on a listing, which has no query to rank.
	Ranking       string `json:"ranking,omitempty"`
	RankingReason string `json:"ranking_reason,omitempty"`
}
type Error struct {
	Code             string `json:"code"`
	Message          string `json:"message"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
	CurrentRevision  int64  `json:"current_revision,omitempty"`
	ID               string `json:"id,omitempty"`
	// Actor names who wrote the revision that caused a conflict, so the
	// refusal can be acted on without a second lookup.
	Actor string `json:"actor,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

type ErrorEnvelope struct {
	Version string `json:"version"`
	Error   *Error `json:"error"`
}
type Filter struct {
	Prefix, Project, Kind, Query, Mode, Cursor string
	IncludeArchived                            bool
	Limit                                      int
}
