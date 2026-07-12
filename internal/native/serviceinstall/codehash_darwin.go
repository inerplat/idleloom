//go:build darwin

package serviceinstall

/*
#include <unistd.h>
#include <sys/types.h>

#define IDLELOOM_CS_OPS_CDHASH 5
#define IDLELOOM_CS_CDHASH_LEN 20

int csops(pid_t pid, unsigned int ops, void *useraddr, size_t usersize);

static int idleloom_self_cdhash(unsigned char *hash) {
	return csops(getpid(), IDLELOOM_CS_OPS_CDHASH, hash, IDLELOOM_CS_CDHASH_LEN);
}
*/
import "C"

import "fmt"

func runningCodeHash() ([]byte, error) {
	result := make([]byte, int(C.IDLELOOM_CS_CDHASH_LEN))
	if status := C.idleloom_self_cdhash((*C.uchar)(&result[0])); status != 0 {
		return nil, fmt.Errorf("csops returned %d", status)
	}
	return result, nil
}
