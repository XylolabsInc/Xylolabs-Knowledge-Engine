import * as account from './account.mjs';
import * as read from './read.mjs';
import * as guide from './guide.mjs';
import * as purchase from './purchase.mjs';
import * as inspection from './inspection.mjs';
import * as claim from './claim.mjs';
import * as trip from './trip.mjs';
import * as cert from './cert.mjs';

/** Every tool module, in listing order. Each exports `register(server, rt)`. */
export const MODULES = [account, read, guide, purchase, inspection, claim, trip, cert];

export function registerAll(server, rt) {
  for (const m of MODULES) m.register(server, rt);
}
