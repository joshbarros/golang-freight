import { test, expect } from '@playwright/test';
import { ADMIN, adminToken, signIn } from './support';

test('open a sailing, book space, see the bill of lading, reverse it', async ({ page }) => {
  const run = Date.now().toString(36).toUpperCase();
  const code = `E2E-${run}`;
  const reference = `BK-${run}`;

  await signIn(page, ADMIN);
  await page.getByLabel('Sailing code').fill(code);
  await page.getByLabel('Capacity (TEU)').fill('8');
  await page.getByRole('button', { name: 'Open sailing' }).click();

  const remaining = page.getByTestId('selected-remaining');
  await expect(page.getByRole('heading', { name: `Book space on ${code}` })).toBeVisible();
  await expect(remaining).toHaveText('8');

  await page.getByLabel('Booking reference').fill(reference);
  await page.getByLabel('TEU', { exact: true }).fill('3');
  await page.getByRole('button', { name: 'Book space' }).click();

  const row = page.locator(`tr[data-reference="${reference}"]`);
  await expect(row.getByTestId('status')).toHaveText('Issued', { timeout: 20_000 });
  await expect(row.getByTestId('bol')).toHaveText(/BL-[0-9A-F]{12}/);
  await expect(remaining).toHaveText('5');

  // Nothing is kept in the browser: a fresh load reads the same state from the API.
  // Nothing sensitive is stored either: the refresh cookie is HttpOnly, so a reload signs back in through it.
  expect(await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }))).toBe(
    '{}',
  );
  await page.reload();
  await expect(page.getByTestId('whoami')).toContainText(ADMIN.email);
  await page.getByRole('button', { name: new RegExp(`^${code}`) }).click();
  const reloaded = page.locator(`tr[data-reference="${reference}"]`);
  await expect(reloaded.getByTestId('status')).toHaveText('Issued');
  await expect(page.getByTestId('selected-remaining')).toHaveText('5');

  await reloaded.getByRole('button', { name: 'Reverse' }).click();
  await expect(reloaded.getByTestId('status')).toHaveText('Reversed');
  await expect(page.getByTestId('selected-remaining')).toHaveText('8');
  // The issued document is not rolled back.
  await expect(reloaded.getByTestId('bol')).toHaveText(/BL-[0-9A-F]{12}/);
  const headers = { Authorization: `Bearer ${await adminToken(page.request)}` };
  const bookings = await page.request.get(`/api/bookings?sailingCode=${code}`, { headers });
  const [booking] = await bookings.json();
  const document = await page.request.get(`/api/documents/${booking.id}`, { headers });
  expect((await document.json()).status).toBe('Issued');
});
