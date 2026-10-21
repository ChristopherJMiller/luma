import { defineConfig } from 'vitest/config'

// The conductor's unit and integration tests. Spike S (spike/) runs only
// through vitest.spike.config.ts (`make sdk-spike`).
export default defineConfig({
  test: {
    include: ['test/**/*.test.ts'],
    exclude: ['spike/**', 'node_modules/**'],
  },
})
