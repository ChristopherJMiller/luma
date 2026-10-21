import { defineConfig } from 'vitest/config'

// Spike S: `make sdk-spike`. Each case drives the real CLI against the
// fakeanthropic binary (bin/fakeanthropic), so cases are slow and run one at
// a time, in file order: the last case (S9) reads every request the others
// made.
export default defineConfig({
  test: {
    include: ['spike/**/*.spike.test.ts'],
    testTimeout: 300_000,
    hookTimeout: 60_000,
    fileParallelism: false,
    sequence: { concurrent: false },
    reporters: ['verbose'],
  },
})
