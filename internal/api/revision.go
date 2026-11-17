package api

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
)

// An instance has a revision (instances.Instance.Revision) that grows with
// every stored change of its settings. A caller that read it, changes
// settings from what it read, and does not want to overwrite a change made
// meanwhile, makes the change with If-Match: "<revision>"; the instance is
// then changed only if it is still at that revision, else the answer is 412
// with the code "changed" and nothing is done. The reads and the change
// answers send the revision as a strong ETag.
//
// A change call checks, in this order: the role of the route, then (for the
// site role, which may send some fields only) the fields of the body, which
// is 403 whatever the If-Match says, the body (400), the If-Match header's
// form (400), the instance (404), the revision (412), and then the change
// itself (422, 409), which is checked against the instance at that
// revision. So a field the caller may not change is refused before the
// revision is looked at, and a refused call tells nothing about whether
// the revision matched.

// IfMatchHeader names the header of a change call that makes it conditional
// on the revision.
const IfMatchHeader = "If-Match"

// revisionTagRe is one strong entity tag that holds a revision: a positive
// whole number without leading zeros, in double quotes. A weak tag (W/"3"),
// a list of tags and anything else is not a revision.
var revisionTagRe = regexp.MustCompile(`^"([1-9][0-9]{0,17})"$`)

// etag is the entity tag of an instance at the revision rev, as sent in the
// ETag header and expected in If-Match.
func etag(rev int64) string { return `"` + strconv.FormatInt(rev, 10) + `"` }

// ifMatch reads the If-Match header of a change call of an instance. No
// header, or "*" (any revision), is no condition. One tag with a revision
// is that revision. Anything else is 400 bad_if_match, and the call is not
// made: a header sent twice, an empty one, a list, a weak tag, a tag that
// is not a revision, and a value that is not a tag at all.
func ifMatch(r *http.Request) (instances.Condition, error) {
	vals := r.Header.Values(IfMatchHeader)
	if len(vals) == 0 {
		return instances.Condition{}, nil
	}
	if len(vals) == 1 {
		v := strings.TrimSpace(vals[0])
		if v == "*" {
			return instances.Condition{}, nil
		}
		if m := revisionTagRe.FindStringSubmatch(v); m != nil {
			rev, err := strconv.ParseInt(m[1], 10, 64)
			if err == nil {
				return instances.Condition{Revision: rev}, nil
			}
		}
	}
	return instances.Condition{}, fault.New(fault.BadRequest, "bad_if_match",
		`%s must be "*" or one revision in double quotes, like "3": the ETag of the instance, sent once (no list, no W/ tag)`, IfMatchHeader)
}

// setETag puts the entity tag of an instance at revision rev in the answer.
func setETag(w http.ResponseWriter, rev int64) {
	w.Header().Set("ETag", etag(rev))
}
