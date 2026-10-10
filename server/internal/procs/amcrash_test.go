package procs

import "testing"

func TestAmCrashPID(t *testing.T) {
	cases := []struct {
		msg       string
		pid, user int
		proc      string
	}{
		// API 29: [pid,user,process,flags,exc,msg,file,line]
		{"[7182,0,com.example.logtest,13155910,java.lang.IllegalStateException,repo not ready,Triggers.java,53]", 7182, 0, "com.example.logtest"},
		// API 30+: [user,pid,...]
		{"[0,7182,com.example.logtest,13155910,java.lang.IllegalStateException,repo not ready,Triggers.java,53]", 7182, 0, "com.example.logtest"},
		// native crash variant carries system_server's pid
		{"[4944,0,com.example.logtest,13155910,Native crash,Segmentation fault,unknown,0]", 4944, 0, "com.example.logtest"},
	}
	for _, c := range cases {
		pid, user, proc, ok := AmCrashPID(c.msg)
		if !ok || pid != c.pid || user != c.user || proc != c.proc {
			t.Errorf("%s → %d %d %q %v", c.msg, pid, user, proc, ok)
		}
	}
	if _, _, _, ok := AmCrashPID("[garbage]"); ok {
		t.Error("garbage parsed")
	}
}
