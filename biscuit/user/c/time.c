#include <litc.h>

ulong now()
{
	struct timeval t;
	int ret;
	if ((ret = gettimeofday(&t, NULL)) < 0)
		err(-1, "gettimeofday");

	// ms
	return t.tv_sec * 1000 + t.tv_usec / 1000;
}

void usage()
{
	errx(-1, "usage: %s [-r] <command> <arg1> ...", __progname);
}

int main(int argc, char **argv)
{
	int profile = 0;
	int ch;
	while ((ch = getopt(argc, argv, "r")) != -1) {
		switch (ch) {
		case 'r':
			profile = 1;
			break;
		default:
			usage();
			break;
		}
	}
	argc -= optind;
	argv += optind;

	if (argc < 1)
		usage();

	ulong start = now();

	// start profiling
	if (profile && fake_sys(1))
		errx(-1, "prof start");

	if (fork() == 0) {
		execvp(argv[0], &argv[0]);
		err(-1, "execv");
	}

	struct rusage r;
	int status;
	int ret = wait4(WAIT_ANY, &status, 0, &r);
	if (ret < 0)
		err(-1, "wait4");
	ulong elapsed = now() - start;

	// stop profiling
	if (profile && fake_sys(0))
		errx(-1, "prof stop");

	if (!WIFEXITED(status) || WEXITSTATUS(status))
		printf("child failed with status: %d\n", WEXITSTATUS(status));

	printf("%lu seconds, %lu ms\n", elapsed/1000, elapsed%1000);
	printf("user   time: %lu seconds, %lu us\n", r.ru_utime.tv_sec,
	    r.ru_utime.tv_usec);
	printf("system time: %lu seconds, %lu us\n", r.ru_stime.tv_sec,
	    r.ru_stime.tv_usec);
	return 0;
}
