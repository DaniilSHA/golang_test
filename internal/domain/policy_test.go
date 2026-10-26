package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestTaskPermissions(t *testing.T) {
	assignee := int64(3)
	task := Task{CreatedBy: 2, AssigneeID: &assignee}
	status := TaskUpdate{Status: Field[string]{Set: true, Value: "done"}}
	title := TaskUpdate{Title: Field[string]{Set: true, Value: "new"}}
	reassign := TaskUpdate{AssigneeID: Field[int64]{Set: true, Null: true}}
	for _, tc := range []struct {
		name    string
		role    Role
		user    int64
		update  TaskUpdate
		allowed bool
	}{
		{"owner edits any title", Owner, 1, title, true},
		{"admin reassigns", Admin, 1, reassign, true},
		{"author edits title", Member, 2, title, true},
		{"author reassigns", Member, 2, reassign, true},
		{"assignee changes status", Member, 3, status, true},
		{"assignee cannot edit title", Member, 3, title, false},
		{"assignee cannot unassign", Member, 3, reassign, false},
		{"unrelated member cannot change status", Member, 4, status, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := AuthorizeUpdate(tc.role, tc.user, task, tc.update)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v, error=%v", tc.allowed, err)
			}
		})
	}
}
func TestMemberPermissions(t *testing.T) {
	for _, tc := range []struct {
		actor, target, requested Role
		status                   int
	}{
		{Owner, Member, Admin, 0}, {Admin, Admin, Member, 0}, {Admin, Member, "", 0},
		{Member, Member, Admin, 403}, {Owner, Owner, Member, 409}, {Admin, Owner, "", 409}, {Owner, Member, Owner, 400},
	} {
		err := AuthorizeMemberChange(tc.actor, tc.target, tc.requested)
		if tc.status == 0 {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Status != tc.status {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}
func TestOptionalAssigneeDistinguishesNullAndMissing(t *testing.T) {
	for _, tc := range []struct {
		raw       string
		set, null bool
		value     int64
	}{
		{`{"version":1}`, false, false, 0}, {`{"version":1,"assignee_id":null}`, true, true, 0}, {`{"version":1,"assignee_id":42}`, true, false, 42},
	} {
		var in TaskUpdate
		if err := json.Unmarshal([]byte(tc.raw), &in); err != nil {
			t.Fatal(err)
		}
		if in.AssigneeID.Set != tc.set || in.AssigneeID.Null != tc.null || in.AssigneeID.Value != tc.value {
			t.Fatalf("%s: %+v", tc.raw, in.AssigneeID)
		}
	}
}
