import { TestBed } from '@angular/core/testing';

import {
  ApplicationConfigFieldsComponent,
  parseDomainRows,
  serializeDomainRows,
  type ConfigSection,
} from './config-fields.component';
import { emptyConfigForm, type ConfigForm } from './application-form';

describe('routing table parse/serialize', () => {
  it('groups domains that share a target port into one row', () => {
    const rows = parseDomainRows('a.example.com:1337\nb.example.com:1337\nc.example.com');
    expect(rows).toEqual([
      { domains: 'a.example.com, b.example.com', port: '1337' },
      { domains: 'c.example.com', port: '' },
    ]);
  });

  it('serializes a comma-separated row to one line per fqdn with the port', () => {
    const out = serializeDomainRows([{ domains: 'a.example.com, b.example.com', port: '8080' }]);
    expect(out).toBe('a.example.com:8080\nb.example.com:8080');
  });

  it('keeps a path (port precedes path) and ignores a non-numeric suffix', () => {
    const rows = parseDomainRows('app.example.com:3000/api\nhost.example.com:notaport');
    expect(rows).toEqual([
      { domains: 'app.example.com/api', port: '3000' },
      { domains: 'host.example.com:notaport', port: '' },
    ]);
  });

  it('round-trips a mixed set', () => {
    const text = 'a.example.com:1337\nb.example.com:1337\nc.example.com/admin';
    expect(serializeDomainRows(parseDomainRows(text))).toBe(text);
  });

  it('drops blank rows and blank domains', () => {
    expect(serializeDomainRows([{ domains: '  ,  ', port: '80' }, { domains: '', port: '' }])).toBe(
      '',
    );
  });
});

/** Mounts the fields with a fresh form; the returned object IS what the parent
 * owns, so the assertions read the contract the component writes back. */
function mount(overrides: Partial<ConfigForm> = {}, section?: ConfigSection) {
  const form: ConfigForm = { ...emptyConfigForm(), ...overrides };
  const fixture = TestBed.createComponent(ApplicationConfigFieldsComponent);
  fixture.componentRef.setInput('form', form);
  fixture.componentRef.setInput('sourceType', 'git');
  fixture.componentRef.setInput('section', section);
  fixture.detectChanges();
  return { fixture, form, host: fixture.nativeElement as HTMLElement };
}

const inputsLabelled = (host: HTMLElement, label: string) =>
  Array.from(host.querySelectorAll<HTMLInputElement>(`input[aria-label="${label}"]`));
const domainInputs = (host: HTMLElement) => inputsLabelled(host, 'Domains for this route');
const portInputs = (host: HTMLElement) => inputsLabelled(host, 'Target port for this route');
const removeButtons = (host: HTMLElement) =>
  Array.from(host.querySelectorAll<HTMLButtonElement>('button[aria-label="Remove route"]'));

function typeInto(input: HTMLInputElement, value: string): void {
  input.value = value;
  input.dispatchEvent(new Event('input'));
}

describe('routing table editor', () => {
  it('offers one empty row to an application that has no domain yet', async () => {
    const { fixture, host } = mount({ domains: '' });
    await fixture.whenStable();

    expect(domainInputs(host).length).toBe(1);
    expect(domainInputs(host)[0].value).toBe('');
  });

  it('seeds one row per target port from the stored domains', async () => {
    const { fixture, host } = mount({
      domains: 'a.example.com:1337\nb.example.com:1337\nc.example.com',
    });
    await fixture.whenStable();

    expect(domainInputs(host).map((i) => i.value)).toEqual([
      'a.example.com, b.example.com',
      'c.example.com',
    ]);
    expect(portInputs(host).map((i) => i.value)).toEqual(['1337', '']);
  });

  // The rows are working state; `form().domains` is the contract the parent
  // saves. An edit that forgot to write back would look right on screen and
  // save the old routing — the failure mode this pins down.
  it('writes an edited row back into the form and leaves the other rows alone', () => {
    const { form, host } = mount({ domains: 'a.example.com\nb.example.com:8080' });

    typeInto(portInputs(host)[0], '3000');

    expect(form.domains).toBe('a.example.com:3000\nb.example.com:8080');
  });

  it('drops a removed row from the form', () => {
    const { fixture, form, host } = mount({ domains: 'a.example.com\nb.example.com:8080' });

    removeButtons(host)[0].click();
    fixture.detectChanges();

    expect(form.domains).toBe('b.example.com:8080');
  });

  // Removing the last route must leave an empty row behind: a table with no
  // row at all has no "add" affordance in it, and the operator would be stuck
  // with an application that can never be routed again.
  it('keeps one empty row when the last route is removed, and clears the form', () => {
    const { fixture, form, host } = mount({ domains: 'only.example.com' });

    removeButtons(host)[0].click();
    fixture.detectChanges();

    expect(form.domains).toBe('');
    expect(domainInputs(host).length).toBe(1);
  });

  // Adding a row deliberately does NOT touch the contract: an empty row is not
  // a route, and writing back here would mark the form dirty for nothing.
  it('adds an empty row without changing the domains', () => {
    const { fixture, form, host } = mount({ domains: 'a.example.com' });

    host.querySelector<HTMLButtonElement>('button.add-route')!.click();
    fixture.detectChanges();

    expect(domainInputs(host).length).toBe(2);
    expect(form.domains).toBe('a.example.com');
  });
});

describe('section menu', () => {
  // Keyed by the union rather than listed by hand: a section added to
  // ConfigSection without a fieldset in the template stops COMPILING here, and
  // the settings menu entry that points at it renders a blank pane otherwise.
  const LEGENDS: Record<ConfigSection, string> = {
    general: 'General',
    source: 'Source',
    build: 'Build',
    routing: 'Routing',
    hooks: 'Deployment hooks',
    health: 'Health check',
    resources: 'Resource limits',
  };

  it('renders exactly the picked section, for every section of the menu', () => {
    for (const section of Object.keys(LEGENDS) as ConfigSection[]) {
      const { host } = mount({}, section);
      const legends = Array.from(host.querySelectorAll('legend')).map((l) => l.textContent!.trim());

      expect(legends.length).withContext(section).toBe(1);
      expect(legends[0]).withContext(section).toContain(LEGENDS[section]);
    }
  });

  it('stacks every section when no single one is picked (the create page)', () => {
    const { host } = mount();

    expect(host.querySelectorAll('legend').length).toBe(Object.keys(LEGENDS).length);
  });
});
