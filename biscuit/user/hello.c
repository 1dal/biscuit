#include <litc.h>

int main() {
	int i;
	for (i = 0; i < 3; i++) {
		pmsg("hello world!");
		int j;
		for (j = 0; j < 100000000; j++)
			asm volatile("":::"memory");
	}
	exit(0);
	return 0;
}