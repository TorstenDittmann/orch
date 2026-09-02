import { job, run } from '@change-codes/ci';

export default job({
	name: 'Go checks',
	steps: [
		run('Verify change.codes checkout', 'test -n "$CHANGE_CODES_SHA"'),
		run('Download dependencies', 'go mod download'),
		run('Run project checks', 'make check')
	]
});
