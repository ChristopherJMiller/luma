package attachments

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// testChildEnv starts this test binary as a misbehaving page counter (see
// withChild); the real one is started by childEnv, before TestMain runs.
const testChildEnv = "LUMA_ATTACHMENTS_TEST_CHILD"

func TestMain(m *testing.M) {
	switch os.Getenv(testChildEnv) {
	case "":
		os.Exit(m.Run())
	case "hang":
		time.Sleep(time.Hour)
	case "crash":
		panic("a crashing page counter")
	case "noise":
		fmt.Println("not a page count")
	case "zero":
		fmt.Println(0)
	case "alloc": // the real child's limit, then more than it allows
		if err := limitChildMemory(); err != nil {
			os.Exit(3)
		}
		b := make([]byte, 512<<20)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		fmt.Println(len(b))
	}
	os.Exit(0)
}
