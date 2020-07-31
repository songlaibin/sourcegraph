import React, { useState, useCallback } from 'react'
import ChevronRightIcon from 'mdi-react/ChevronRightIcon'

export interface BreadcrumbsProps {
    breadcrumbs: React.ReactNode[]
}

export interface UpdateBreadcrumbsProps {
    pushBreadcrumb: (element: React.ReactNode) => () => void
}

export const useBreadcrumbs = (): BreadcrumbsProps & UpdateBreadcrumbsProps => {
    const [breadcrumbs, setBreadcrumbs] = useState<React.ReactNode[]>([])
    const pushBreadcrumb = useCallback(
        (element: React.ReactNode) => {
            setBreadcrumbs(breadcrumbs => [...breadcrumbs, element])
            return () => setBreadcrumbs(breadcrumbs => breadcrumbs.filter(breadcrumb => breadcrumb !== element))
        },
        [setBreadcrumbs]
    )
    return {
        breadcrumbs,
        pushBreadcrumb,
    }
}

export const Breadcrumbs: React.FunctionComponent<BreadcrumbsProps> = props => {
    const { breadcrumbs } = props
    return (
        <>
            {breadcrumbs.map((breadcrumb, index) => (
                <>
                    {index !== 0 && <ChevronRightIcon />} {breadcrumb}
                </>
            ))}
        </>
    )
}