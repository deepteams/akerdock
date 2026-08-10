import { TestBed } from '@angular/core/testing';

import { TimezoneSelectComponent } from './timezone-select.component';

function mount(value = 'UTC') {
  const fixture = TestBed.createComponent(TimezoneSelectComponent);
  fixture.componentRef.setInput('value', value);
  fixture.detectChanges();
  return fixture;
}

function openPanel(fixture: ReturnType<typeof mount>): HTMLElement {
  const host: HTMLElement = fixture.nativeElement;
  host.querySelector('button')!.dispatchEvent(new MouseEvent('click', { bubbles: true }));
  fixture.detectChanges();
  return host;
}

function type(host: HTMLElement, fixture: ReturnType<typeof mount>, text: string): void {
  const search = host.querySelector<HTMLInputElement>('input')!;
  search.value = text;
  search.dispatchEvent(new Event('input'));
  fixture.detectChanges();
}

describe('timezone select', () => {
  it('shows the selected zone with its current offset', () => {
    const host: HTMLElement = mount('Europe/Paris').nativeElement;

    expect(host.querySelector('.trigger .zone')!.textContent).toContain('Europe/Paris');
    expect(host.querySelector('.trigger .offset')!.textContent).toMatch(/^UTC[+-]\d\d:\d\d$/);
  });

  it('stays closed until the trigger is clicked', () => {
    const fixture = mount();

    expect(fixture.nativeElement.querySelector('[role="listbox"]')).toBeNull();

    const host = openPanel(fixture);

    expect(host.querySelector('[role="listbox"]')).not.toBeNull();
    // The engine's database, not a bundled table.
    expect(host.querySelectorAll('[role="option"]').length).toBeGreaterThan(30);
  });

  it('offers UTC first', () => {
    const host = openPanel(mount('Europe/Paris'));
    const first = host.querySelector<HTMLElement>('[role="option"]')!;

    expect(first.querySelector('.zone')!.textContent!.trim()).toBe('UTC');
  });

  it('marks the current value as selected', () => {
    const host = openPanel(mount('UTC'));
    const first = host.querySelector<HTMLElement>('[role="option"]')!;

    expect(first.getAttribute('aria-selected')).toBe('true');
  });

  it('filters the list as the operator types', () => {
    const fixture = mount();
    const host = openPanel(fixture);
    type(host, fixture, 'paris');

    const zones = [...host.querySelectorAll('[role="option"] .zone')].map((el) =>
      el.textContent!.trim(),
    );

    expect(zones).toContain('Europe/Paris');
    expect(zones).not.toContain('Asia/Tokyo');
  });

  it('says so when nothing matches', () => {
    const fixture = mount();
    const host = openPanel(fixture);
    type(host, fixture, 'nowhere-at-all');

    expect(host.querySelectorAll('[role="option"]').length).toBe(0);
    expect(host.querySelector('.empty')!.textContent).toContain('No timezone matches');
  });

  it('emits the IANA name and closes', () => {
    const fixture = mount();
    const emitted: string[] = [];
    fixture.componentInstance.valueChange.subscribe((zone: string) => emitted.push(zone));

    const host = openPanel(fixture);
    type(host, fixture, 'europe/paris');
    host.querySelector<HTMLButtonElement>('[role="option"]')!.click();
    fixture.detectChanges();

    expect(emitted).toEqual(['Europe/Paris']);
    expect(host.querySelector('[role="listbox"]')).toBeNull();
  });

  it('does not re-emit the value already selected', () => {
    const fixture = mount('UTC');
    const emitted: string[] = [];
    fixture.componentInstance.valueChange.subscribe((zone: string) => emitted.push(zone));

    const host = openPanel(fixture);
    host.querySelector<HTMLButtonElement>('[role="option"]')!.click();
    fixture.detectChanges();

    expect(emitted).toEqual([]);
    expect(host.querySelector('[role="listbox"]')).toBeNull();
  });

  // Inside a drawer form, Enter must pick a zone — never submit the form.
  it('picks the first match on Enter instead of submitting', () => {
    const fixture = mount();
    const emitted: string[] = [];
    fixture.componentInstance.valueChange.subscribe((zone: string) => emitted.push(zone));

    const host = openPanel(fixture);
    type(host, fixture, 'europe/paris');
    const event = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true });
    host.querySelector('input')!.dispatchEvent(event);
    fixture.detectChanges();

    expect(emitted).toEqual(['Europe/Paris']);
    expect(event.defaultPrevented).toBeTrue();
  });

  it('swallows Enter when nothing matches', () => {
    const fixture = mount();
    const emitted: string[] = [];
    fixture.componentInstance.valueChange.subscribe((zone: string) => emitted.push(zone));

    const host = openPanel(fixture);
    type(host, fixture, 'nowhere-at-all');
    host
      .querySelector('input')!
      .dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    fixture.detectChanges();

    expect(emitted).toEqual([]);
  });

  it('keeps the panel open when the click lands inside it', () => {
    const fixture = mount();
    const host = openPanel(fixture);
    host.querySelector('.panel')!.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    fixture.detectChanges();

    expect(host.querySelector('[role="listbox"]')).not.toBeNull();
  });

  it('closes on a click outside and on Escape', () => {
    const fixture = mount();
    const host = openPanel(fixture);
    document.body.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    fixture.detectChanges();

    expect(host.querySelector('[role="listbox"]')).toBeNull();

    openPanel(fixture);
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    fixture.detectChanges();

    expect(host.querySelector('[role="listbox"]')).toBeNull();
  });

  // The field is often the last one of a drawer whose body scrolls: a panel
  // opened downwards there lands off-screen.
  it('drops the panel upwards when there is no room below', () => {
    const fixture = mount();
    const host: HTMLElement = fixture.nativeElement;
    host.style.position = 'fixed';
    host.style.left = '0';
    host.style.top = `${window.innerHeight - 24}px`;
    openPanel(fixture);

    expect(host.querySelector('.panel')!.classList).toContain('panel--up');
  });

  it('drops the panel downwards when there is room', () => {
    const fixture = mount();
    const host: HTMLElement = fixture.nativeElement;
    host.style.position = 'fixed';
    host.style.left = '0';
    host.style.top = '0';
    openPanel(fixture);

    expect(host.querySelector('.panel')!.classList).not.toContain('panel--up');
  });

  it('never opens while disabled', () => {
    const fixture = mount();
    fixture.componentRef.setInput('disabled', true);
    fixture.detectChanges();
    const host = openPanel(fixture);

    expect(host.querySelector('[role="listbox"]')).toBeNull();
  });

  it('takes the id a sibling label points at', () => {
    const fixture = mount();
    fixture.componentRef.setInput('inputId', 'tk-tz');
    fixture.detectChanges();

    expect(fixture.nativeElement.querySelector('button').id).toBe('tk-tz');
  });
});
