import { ButtonLink, Kicker, useTitle } from '../components/ui'

export default function NotFound() {
  useTitle('Page not found')
  return (
    <div className="py-8 sm:py-16">
      <Kicker>404</Kicker>
      <h1 className="mb-4 mt-4 font-serif text-[44px] font-normal leading-[1.05] tracking-[-0.02em] sm:text-[64px]">This page isn't here.</h1>
      <p className="max-w-xl text-[17px] leading-relaxed text-ink-2 sm:text-xl sm:leading-7">The link is out of date, or the page doesn't exist.</p>
      <div className="mt-8 flex flex-wrap gap-3">
        <ButtonLink to="/">Your apps</ButtonLink>
        <ButtonLink to="/new-app" variant="secondary">
          New app
        </ButtonLink>
      </div>
    </div>
  )
}
